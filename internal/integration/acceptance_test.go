package integration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	mysqldrv "github.com/go-sql-driver/mysql"

	"seckill_lite/internal/config"
	"seckill_lite/internal/model"
	"seckill_lite/internal/mysqlrepo"
	"seckill_lite/internal/redisrepo"
	"seckill_lite/internal/service"
	"seckill_lite/internal/worker"
)

func TestA1ConcurrentNoOversell(t *testing.T) {
	store, gate := setup(t, time.Hour)
	item := createItem(t, store, gate, 100, 1)
	const requests = 1000
	var wg sync.WaitGroup
	var mu sync.Mutex
	var accepted []redisrepo.ReserveRequest
	for i := 0; i < requests; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := redisrepo.ReserveRequest{
				UserID: int64(100000 + i), Key: fmt.Sprintf("it-a1-%d-%d", time.Now().UnixNano(), i),
				PayloadHash: service.PayloadHash(item.ID, 1), ReservationID: fmt.Sprintf("r-%d-%d", time.Now().UnixNano(), i),
				ItemID: item.ID, Quantity: 1, AcceptedAt: time.Now(), TTL: time.Hour, MaxEntries: 10000,
			}
			res, err := gate.Reserve(context.Background(), req)
			if err != nil {
				t.Errorf("reserve: %v", err)
				return
			}
			if res.Code == "ACCEPTED" {
				mu.Lock()
				accepted = append(accepted, req)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(accepted) != 100 {
		t.Fatalf("accepted=%d, want 100", len(accepted))
	}
	var handleWG sync.WaitGroup
	for _, req := range accepted {
		req := req
		handleWG.Add(1)
		go func() {
			defer handleWG.Done()
			_, err := store.HandleReservation(context.Background(), mysqlrepo.ReservationInput{
				ReservationID: req.ReservationID, UserID: req.UserID,
				IdempotencyKey: req.Key, ItemID: req.ItemID, Quantity: 1, PayloadHash: req.PayloadHash,
			})
			if err != nil {
				t.Errorf("handle: %v", err)
			}
		}()
	}
	handleWG.Wait()
	count, err := store.CountOrdersByItem(context.Background(), item.ID)
	if err != nil {
		t.Fatal(err)
	}
	latest, err := store.GetItem(context.Background(), item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if count != 100 || latest.AvailableStock != 0 {
		t.Fatalf("orders=%d stock=%d, want 100/0", count, latest.AvailableStock)
	}
}

func TestA2SameKeyOneOrder(t *testing.T) {
	store, gate := setup(t, time.Hour)
	item := createItem(t, store, gate, 10, 1)
	key := fmt.Sprintf("it-a2-%d", time.Now().UnixNano())
	hash := service.PayloadHash(item.ID, 1)
	var accepted redisrepo.ReserveRequest
	var count int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := redisrepo.ReserveRequest{
				UserID: 7001, Key: key, PayloadHash: hash, ReservationID: fmt.Sprintf("r-a2-%d-%d", time.Now().UnixNano(), i),
				ItemID: item.ID, Quantity: 1, AcceptedAt: time.Now(), TTL: time.Hour, MaxEntries: 10000,
			}
			res, err := gate.Reserve(context.Background(), req)
			if err != nil {
				t.Errorf("reserve: %v", err)
				return
			}
			if res.Code == "ACCEPTED" {
				mu.Lock()
				count++
				accepted = req
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if count != 1 {
		t.Fatalf("accepted=%d, want 1", count)
	}
	state, err := gate.GetRequest(context.Background(), 7001, key)
	if err != nil || state == nil {
		t.Fatalf("state: %v %v", state, err)
	}
	accepted.ReservationID = state.ReservationID
	out, err := store.HandleReservation(context.Background(), mysqlrepo.ReservationInput{
		ReservationID: accepted.ReservationID, UserID: accepted.UserID,
		IdempotencyKey: accepted.Key, ItemID: accepted.ItemID, Quantity: accepted.Quantity, PayloadHash: accepted.PayloadHash,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Order == nil {
		t.Fatalf("expected order, got %+v", out)
	}
	again, err := store.HandleReservation(context.Background(), mysqlrepo.ReservationInput{
		ReservationID: accepted.ReservationID, UserID: accepted.UserID,
		IdempotencyKey: accepted.Key, ItemID: accepted.ItemID, Quantity: accepted.Quantity, PayloadHash: accepted.PayloadHash,
	})
	if err != nil {
		t.Fatal(err)
	}
	if again.Order == nil || again.Order.OrderNo != out.Order.OrderNo {
		t.Fatalf("duplicate result mismatch: %+v vs %+v", again.Order, out.Order)
	}
	orders, err := store.CountOrdersByUserItem(context.Background(), 7001, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if orders != 1 {
		t.Fatalf("orders=%d, want 1", orders)
	}
}

func TestA2bFirstPurchaseLimit(t *testing.T) {
	store, gate := setup(t, time.Hour)
	item := createItem(t, store, gate, 10, 2)
	ctx := context.Background()
	handle := func(user int64, key string, qty int) mysqlrepo.ReservationOutcome {
		t.Helper()
		res, err := gate.Reserve(ctx, redisrepo.ReserveRequest{
			UserID: user, Key: key, PayloadHash: service.PayloadHash(item.ID, qty),
			ReservationID: fmt.Sprintf("r-limit-%d-%d", user, time.Now().UnixNano()),
			ItemID:        item.ID, Quantity: qty, AcceptedAt: time.Now(), TTL: time.Hour, MaxEntries: 10000,
		})
		if err != nil || res.Code != "ACCEPTED" {
			t.Fatalf("reserve qty=%d: %+v %v", qty, res, err)
		}
		state, err := gate.GetRequest(ctx, user, key)
		if err != nil || state == nil {
			t.Fatalf("state: %v %v", state, err)
		}
		out, err := store.HandleReservation(ctx, mysqlrepo.ReservationInput{
			ReservationID: state.ReservationID, UserID: user, IdempotencyKey: key,
			ItemID: item.ID, Quantity: qty, PayloadHash: service.PayloadHash(item.ID, qty),
		})
		if err != nil {
			t.Fatalf("handle qty=%d: %v", qty, err)
		}
		return out
	}

	if out := handle(8101, fmt.Sprintf("it-limit-over-%d", time.Now().UnixNano()), 3); out.Compensate != true {
		t.Fatalf("first over-limit must be rejected: %+v", out)
	}
	if out := handle(8102, fmt.Sprintf("it-limit-first-%d", time.Now().UnixNano()), 2); out.Order == nil {
		t.Fatalf("first at-limit must succeed: %+v", out)
	}
	user := int64(8103)
	if out := handle(user, fmt.Sprintf("it-limit-a-%d", time.Now().UnixNano()), 1); out.Order == nil {
		t.Fatalf("first purchase: %+v", out)
	}
	if out := handle(user, fmt.Sprintf("it-limit-b-%d", time.Now().UnixNano()), 1); out.Order == nil {
		t.Fatalf("second purchase up to limit: %+v", out)
	}
	if out := handle(user, fmt.Sprintf("it-limit-c-%d", time.Now().UnixNano()), 1); out.Order != nil {
		t.Fatalf("third purchase must be rejected: %+v", out)
	}
	orders, err := store.CountOrdersByUserItem(ctx, user, item.ID)
	if err != nil || orders != 2 {
		t.Fatalf("orders=%d err=%v, want 2", orders, err)
	}
}

func TestA3CancelRestockOnce(t *testing.T) {
	store, gate := setup(t, time.Millisecond)
	drainExpiredOrders(t, store, gate)
	item := createItem(t, store, gate, 1, 1)
	key := fmt.Sprintf("it-a3-%d", time.Now().UnixNano())
	hash := service.PayloadHash(item.ID, 1)
	req := redisrepo.ReserveRequest{
		UserID: 7002, Key: key, PayloadHash: hash, ReservationID: fmt.Sprintf("r-a3-%d", time.Now().UnixNano()),
		ItemID: item.ID, Quantity: 1, AcceptedAt: time.Now(), TTL: time.Hour, MaxEntries: 10000,
	}
	res, err := gate.Reserve(context.Background(), req)
	if err != nil || res.Code != "ACCEPTED" {
		t.Fatalf("reserve: %+v %v", res, err)
	}
	out, err := store.HandleReservation(context.Background(), mysqlrepo.ReservationInput{
		ReservationID: req.ReservationID, UserID: req.UserID,
		IdempotencyKey: req.Key, ItemID: req.ItemID, Quantity: 1, PayloadHash: req.PayloadHash,
	})
	if err != nil || out.Order == nil {
		t.Fatalf("handle: %+v %v", out, err)
	}
	time.Sleep(20 * time.Millisecond)
	svc := service.New(store, gate, config.Config{CancelBatch: 10})
	n, err := svc.CancelExpired(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("first cancel n=%d err=%v, want 1 after draining", n, err)
	}
	n, err = svc.CancelExpired(context.Background())
	if err != nil || n != 0 {
		t.Fatalf("second cancel n=%d err=%v", n, err)
	}
	latest, err := store.GetItem(context.Background(), item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if latest.AvailableStock != 1 {
		t.Fatalf("available stock=%d, want 1", latest.AvailableStock)
	}
	logs, err := store.CountStockLogs(context.Background(), out.Order.OrderNo, "CANCEL")
	if err != nil {
		t.Fatal(err)
	}
	if logs != 1 {
		t.Fatalf("cancel logs=%d, want 1", logs)
	}
	stock, found, err := gate.GetStock(context.Background(), item.ID)
	if err != nil || !found || stock != 1 {
		t.Fatalf("redis stock=%d found=%v err=%v, want 1", stock, found, err)
	}
}

func TestA4WorkerReclaimsPending(t *testing.T) {
	store, gate := setup(t, time.Hour)
	item := createItem(t, store, gate, 1, 1)
	key := fmt.Sprintf("it-a4-%d", time.Now().UnixNano())
	req := redisrepo.ReserveRequest{
		UserID: 7003, Key: key, PayloadHash: service.PayloadHash(item.ID, 1),
		ReservationID: fmt.Sprintf("r-a4-%d", time.Now().UnixNano()), ItemID: item.ID,
		Quantity: 1, AcceptedAt: time.Now(), TTL: time.Hour, MaxEntries: 10000,
	}
	res, err := gate.Reserve(context.Background(), req)
	if err != nil || res.Code != "ACCEPTED" {
		t.Fatalf("reserve: %+v %v", res, err)
	}
	msgs, err := gate.ReadGroup(context.Background(), "consumer-a", 10, time.Millisecond)
	if err != nil || len(msgs) == 0 {
		t.Fatalf("read group: %v len=%d", err, len(msgs))
	}
	claimed, err := gate.AutoClaim(context.Background(), "consumer-b", 0, 10)
	if err != nil || len(claimed) == 0 {
		t.Fatalf("autoclaim: %v len=%d", err, len(claimed))
	}
	wk := worker.New(store, gate, time.Hour)
	if err := wk.Process(context.Background(), claimed[0]); err != nil {
		t.Fatalf("process: %v", err)
	}
	if err := gate.Ack(context.Background(), claimed[0].ID); err != nil {
		t.Fatal(err)
	}
	// A duplicate delivery must not create a second order.
	if err := wk.Process(context.Background(), claimed[0]); err != nil {
		t.Fatalf("duplicate process: %v", err)
	}
	count, err := store.CountOrdersByItem(context.Background(), item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("orders=%d, want 1", count)
	}
}

func TestPaymentIdempotentAndAfterCancel(t *testing.T) {
	store, gate := setup(t, time.Millisecond)
	drainExpiredOrders(t, store, gate)
	item := createItem(t, store, gate, 1, 1)
	key := fmt.Sprintf("it-a5-%d", time.Now().UnixNano())
	req := redisrepo.ReserveRequest{
		UserID: 7004, Key: key, PayloadHash: service.PayloadHash(item.ID, 1),
		ReservationID: fmt.Sprintf("r-a5-%d", time.Now().UnixNano()), ItemID: item.ID,
		Quantity: 1, AcceptedAt: time.Now(), TTL: time.Hour, MaxEntries: 10000,
	}
	if res, err := gate.Reserve(context.Background(), req); err != nil || res.Code != "ACCEPTED" {
		t.Fatalf("reserve: %+v %v", res, err)
	}
	out, err := store.HandleReservation(context.Background(), mysqlrepo.ReservationInput{
		ReservationID: req.ReservationID, UserID: req.UserID, IdempotencyKey: req.Key,
		ItemID: req.ItemID, Quantity: 1, PayloadHash: req.PayloadHash,
	})
	if err != nil || out.Order == nil {
		t.Fatalf("order: %+v %v", out, err)
	}
	if _, err := store.MarkPaid(context.Background(), 7004, out.Order.OrderNo, "txn-a", out.Order.AmountCents+1); !errors.Is(err, mysqlrepo.ErrPaymentAmount) {
		t.Fatalf("amount mismatch err=%v", err)
	}
	paid, err := store.MarkPaid(context.Background(), 7004, out.Order.OrderNo, "txn-a", out.Order.AmountCents)
	if err != nil || paid.Status != "PAID" {
		t.Fatalf("pay: %+v %v", paid, err)
	}
	paidAgain, err := store.MarkPaid(context.Background(), 7004, out.Order.OrderNo, "txn-a", out.Order.AmountCents)
	if err != nil || paidAgain.Status != "PAID" {
		t.Fatalf("idempotent pay: %+v %v", paidAgain, err)
	}

	item2 := createItem(t, store, gate, 1, 1)
	key2 := fmt.Sprintf("it-a5-cancel-%d", time.Now().UnixNano())
	req2 := redisrepo.ReserveRequest{
		UserID: 7005, Key: key2, PayloadHash: service.PayloadHash(item2.ID, 1),
		ReservationID: fmt.Sprintf("r-a5c-%d", time.Now().UnixNano()), ItemID: item2.ID,
		Quantity: 1, AcceptedAt: time.Now(), TTL: time.Hour, MaxEntries: 10000,
	}
	if res, err := gate.Reserve(context.Background(), req2); err != nil || res.Code != "ACCEPTED" {
		t.Fatalf("reserve2: %+v %v", res, err)
	}
	out2, err := store.HandleReservation(context.Background(), mysqlrepo.ReservationInput{
		ReservationID: req2.ReservationID, UserID: req2.UserID, IdempotencyKey: req2.Key,
		ItemID: req2.ItemID, Quantity: 1, PayloadHash: req2.PayloadHash,
	})
	if err != nil || out2.Order == nil {
		t.Fatalf("order2: %+v %v", out2, err)
	}
	time.Sleep(20 * time.Millisecond)
	svc := service.New(store, gate, config.Config{CancelBatch: 10})
	if n, err := svc.CancelExpired(context.Background()); err != nil || n != 1 {
		t.Fatalf("cancel: n=%d err=%v", n, err)
	}
	if _, err := store.MarkPaid(context.Background(), 7005, out2.Order.OrderNo, "txn-b", out2.Order.AmountCents); !errors.Is(err, mysqlrepo.ErrPayAfterCancel) {
		t.Fatalf("pay after cancel err=%v", err)
	}
}

func TestRejectionCompensationOnce(t *testing.T) {
	store, gate := setup(t, time.Hour)
	item := createItem(t, store, gate, 10, 1)
	key := fmt.Sprintf("it-a6-%d", time.Now().UnixNano())
	req := redisrepo.ReserveRequest{
		UserID: 7006, Key: key, PayloadHash: service.PayloadHash(item.ID, 3),
		ReservationID: fmt.Sprintf("r-a6-%d", time.Now().UnixNano()), ItemID: item.ID,
		Quantity: 3, AcceptedAt: time.Now(), TTL: time.Hour, MaxEntries: 10000,
	}
	if res, err := gate.Reserve(context.Background(), req); err != nil || res.Code != "ACCEPTED" {
		t.Fatalf("reserve: %+v %v", res, err)
	}
	out, err := store.HandleReservation(context.Background(), mysqlrepo.ReservationInput{
		ReservationID: req.ReservationID, UserID: req.UserID, IdempotencyKey: req.Key,
		ItemID: req.ItemID, Quantity: req.Quantity, PayloadHash: req.PayloadHash,
	})
	if err != nil || !out.Compensate || out.Order != nil {
		t.Fatalf("expected rejected reservation: %+v %v", out, err)
	}
	if stock, _, _ := gate.GetStock(context.Background(), item.ID); stock != 7 {
		t.Fatalf("redis stock before compensation=%d, want 7", stock)
	}
	wk := worker.New(store, gate, time.Hour)
	if err := wk.Process(context.Background(), redisrepo.Message{
		ID: "it-msg", ReservationID: req.ReservationID, IdempotencyKey: req.Key,
		UserID: req.UserID, ItemID: req.ItemID, Quantity: req.Quantity, PayloadHash: req.PayloadHash,
	}); err != nil {
		t.Fatalf("compensation: %v", err)
	}
	if stock, _, _ := gate.GetStock(context.Background(), item.ID); stock != 10 {
		t.Fatalf("redis stock after compensation=%d, want 10", stock)
	}
	rr, err := store.GetReservationResult(context.Background(), req.ReservationID)
	if err != nil || rr.CompensationState != "DONE" {
		t.Fatalf("reservation result: %+v %v", rr, err)
	}
	// A repeated delivery of the now DONE reservation must not compensate again.
	if err := wk.Process(context.Background(), redisrepo.Message{
		ID: "it-msg", ReservationID: req.ReservationID, IdempotencyKey: req.Key,
		UserID: req.UserID, ItemID: req.ItemID, Quantity: req.Quantity, PayloadHash: req.PayloadHash,
	}); err != nil {
		t.Fatalf("duplicate compensation: %v", err)
	}
	if stock, _, _ := gate.GetStock(context.Background(), item.ID); stock != 10 {
		t.Fatalf("redis stock after duplicate=%d, want 10", stock)
	}
}

func TestServiceEndToEndFlow(t *testing.T) {
	store, gate := setup(t, time.Hour)
	item := createItem(t, store, gate, 2, 1)
	cfg := config.Config{
		IngressRPS: 1000, IngressBurst: 1000,
		UserRPS: 1000, UserBurst: 1000,
		QueryRPS: 1000, QueryBurst: 1000,
		RequestTTL: time.Hour, StreamMax: 10000, CancelBatch: 10,
	}
	svc := service.New(store, gate, cfg)
	ctx := context.Background()
	key := fmt.Sprintf("it-a7-%d", time.Now().UnixNano())
	userID := int64(700000000 + time.Now().UnixNano()%1000000000)
	res, err := svc.Seckill(ctx, userID, key, item.ID, 1)
	if err != nil || res.HTTPStatus != 202 {
		t.Fatalf("seckill: %+v %v", res, err)
	}
	msgs, err := gate.ReadGroup(ctx, "a7-consumer", 10, time.Millisecond)
	if err != nil || len(msgs) == 0 {
		t.Fatalf("read: %v len=%d", err, len(msgs))
	}
	wk := worker.New(store, gate, time.Hour)
	if err := wk.Process(ctx, msgs[0]); err != nil {
		t.Fatalf("process: %v", err)
	}
	if err := gate.Ack(ctx, msgs[0].ID); err != nil {
		t.Fatal(err)
	}
	status, err := svc.GetRequestStatus(ctx, userID, key)
	if err != nil || status.HTTPStatus != 200 || status.Order == nil {
		t.Fatalf("status: %+v %v", status, err)
	}
	order, err := svc.GetOrder(ctx, userID, status.Order.OrderNo)
	if err != nil || order.ItemID != item.ID {
		t.Fatalf("order: %+v %v", order, err)
	}
	orders, err := svc.ListOrders(ctx, userID, 10, 0)
	if err != nil || len(orders) != 1 {
		t.Fatalf("list: %d %v", len(orders), err)
	}
	paid, err := svc.PayMock(ctx, userID, order.OrderNo, fmt.Sprintf("txn-%d", time.Now().UnixNano()), order.AmountCents)
	if err != nil || paid.Status != "PAID" {
		t.Fatalf("pay: %+v %v", paid, err)
	}
	replay, err := svc.Seckill(ctx, userID, key, item.ID, 1)
	if err != nil || replay.Order == nil || replay.Order.OrderNo != order.OrderNo {
		t.Fatalf("replay after payment: %+v %v", replay, err)
	}
	if _, err := svc.Seckill(ctx, userID, key, item.ID, 2); err == nil || err.(*service.AppError).Code != "IDEMPOTENCY_CONFLICT" {
		t.Fatalf("payload conflict: %v", err)
	}
}

func TestDBFinalGuardAgainstRedisOverAdmit(t *testing.T) {
	store, gate := setup(t, time.Hour)
	item := createItem(t, store, gate, 1, 1)
	// Simulate Redis drift so both requests pass the gate; MySQL must reject the second.
	if err := gate.Client().IncrBy(context.Background(), gate.StockKey(item.ID), 1).Err(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	outcomes := make([]mysqlrepo.ReservationOutcome, 0, 2)
	for i := 0; i < 2; i++ {
		user := int64(7020 + i)
		key := fmt.Sprintf("it-a8-%d-%d", time.Now().UnixNano(), i)
		req := redisrepo.ReserveRequest{
			UserID: user, Key: key, PayloadHash: service.PayloadHash(item.ID, 1),
			ReservationID: fmt.Sprintf("r-a8-%d-%d", time.Now().UnixNano(), i), ItemID: item.ID,
			Quantity: 1, AcceptedAt: time.Now(), TTL: time.Hour, MaxEntries: 10000,
		}
		res, err := gate.Reserve(ctx, req)
		if err != nil || res.Code != "ACCEPTED" {
			t.Fatalf("reserve %d: %+v %v", i, res, err)
		}
		out, err := store.HandleReservation(ctx, mysqlrepo.ReservationInput{
			ReservationID: req.ReservationID, UserID: user, IdempotencyKey: key,
			ItemID: item.ID, Quantity: 1, PayloadHash: req.PayloadHash,
		})
		if err != nil {
			t.Fatal(err)
		}
		outcomes = append(outcomes, out)
	}
	if outcomes[0].Order == nil || outcomes[1].Order != nil || !outcomes[1].Compensate {
		t.Fatalf("outcomes: %+v %+v", outcomes[0], outcomes[1])
	}
	count, _ := store.CountOrdersByItem(ctx, item.ID)
	latest, _ := store.GetItem(ctx, item.ID)
	if count != 1 || latest.AvailableStock != 0 {
		t.Fatalf("orders=%d stock=%d, want 1/0", count, latest.AvailableStock)
	}
}

func setup(t *testing.T, expire time.Duration) (*mysqlrepo.Store, *redisrepo.Gateway) {
	t.Helper()
	if os.Getenv("SECKILL_IT") != "1" {
		t.Skip("set SECKILL_IT=1 to run integration tests")
	}
	dsn := os.Getenv("SECKILL_IT_DSN")
	if dsn == "" {
		t.Fatal("SECKILL_IT_DSN must be set to a dedicated *_test database (for example seckill_test)")
	}
	cfg, err := mysqldrv.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("invalid SECKILL_IT_DSN: %v", err)
	}
	if !strings.HasSuffix(cfg.DBName, "_test") {
		t.Fatalf("refusing to run integration tests against non-test database %q; use a *_test database", cfg.DBName)
	}
	store, err := mysqlrepo.Open(dsn, expire)
	if err != nil {
		t.Fatal(err)
	}
	prefix := fmt.Sprintf("sk:it:%d:", time.Now().UnixNano())
	redisAddr := os.Getenv("SECKILL_IT_REDIS")
	if redisAddr == "" {
		redisAddr = "127.0.0.1:6379"
	}
	rawDB := os.Getenv("SECKILL_IT_REDIS_DB")
	if rawDB == "" {
		t.Fatal("SECKILL_IT_REDIS_DB must be set to a dedicated logical DB (for example 15)")
	}
	redisDB, err := strconv.Atoi(rawDB)
	if err != nil || redisDB < 0 {
		t.Fatalf("invalid SECKILL_IT_REDIS_DB %q", rawDB)
	}
	gate := redisrepo.New(redisAddr, "", redisDB, prefix, prefix+"stream:orders", "it-workers", time.Hour)
	if err := gate.EnsureGroup(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		keys, _ := gate.Client().Keys(context.Background(), prefix+"*").Result()
		if len(keys) > 0 {
			_ = gate.Client().Del(context.Background(), keys...).Err()
		}
		gate.Close()
		store.Close()
	})
	return store, gate
}

func createItem(t *testing.T, store *mysqlrepo.Store, gate *redisrepo.Gateway, stock, limit int) model.Item {
	t.Helper()
	now := time.Now().UTC()
	activity, items, err := store.CreateActivity(context.Background(), model.ActivityInput{
		Name: fmt.Sprintf("IT %d", time.Now().UnixNano()), StartAt: now.Add(-time.Minute), EndAt: now.Add(time.Hour),
		Items: []model.ItemInput{{SKUCode: fmt.Sprintf("IT-%d", time.Now().UnixNano()), Name: "it", PriceCents: 100, TotalStock: stock, PerUserLimit: limit}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = activity
	item := items[0]
	if err := gate.SetStockNX(context.Background(), item.ID, stock); err != nil {
		t.Fatal(err)
	}
	if err := gate.SetItemSnapshot(context.Background(), item, time.Minute); err != nil {
		t.Fatal(err)
	}
	return item
}

// drainExpiredOrders cancels any already-expired PENDING_PAYMENT orders before a
// cancellation test starts. CancelExpired scans the whole test database, so
// without draining, leftovers from earlier runs would be processed alongside the
// order under test. Integration tests run serially against a dedicated *_test
// database; this helper makes that assumption explicit and repeatable.
func drainExpiredOrders(t *testing.T, store *mysqlrepo.Store, gate *redisrepo.Gateway) {
	t.Helper()
	svc := service.New(store, gate, config.Config{CancelBatch: 100})
	for i := 0; i < 1000; i++ {
		n, err := svc.CancelExpired(context.Background())
		if err != nil {
			t.Fatalf("drain expired orders: %v", err)
		}
		if n == 0 {
			return
		}
	}
	t.Fatal("drain expired orders did not converge")
}
