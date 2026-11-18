package integration

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"seckill_lite/internal/config"
	"seckill_lite/internal/model"
	"seckill_lite/internal/mysqlrepo"
	"seckill_lite/internal/redisrepo"
	"seckill_lite/internal/service"
	"seckill_lite/internal/worker"
)

func itConfig() config.Config {
	return config.Config{
		IngressRPS:     100000,
		IngressBurst:   100000,
		UserRPS:        100000,
		UserBurst:      100000,
		QueryRPS:       100000,
		QueryBurst:     100000,
		RequestTTL:     time.Hour,
		StreamMax:      10000,
		CancelBatch:    100,
		HistoryLookups: 32,
	}
}

func assertAppCode(t *testing.T, err error, code string) *service.AppError {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error %s, got nil", code)
	}
	appErr, ok := service.AsAppError(err)
	if !ok || appErr.Code != code {
		t.Fatalf("error=%v, want code %s", err, code)
	}
	return appErr
}

func reserveOne(t *testing.T, gate *redisrepo.Gateway, item model.Item, userID int64, key string, quantity int) redisrepo.ReserveRequest {
	t.Helper()
	req := redisrepo.ReserveRequest{
		UserID:        userID,
		Key:           key,
		PayloadHash:   service.PayloadHash(item.ID, quantity),
		ReservationID: fmt.Sprintf("r-%d-%d", item.ID, time.Now().UnixNano()),
		ItemID:        item.ID,
		Quantity:      quantity,
		AcceptedAt:    time.Now(),
		TTL:           time.Hour,
		MaxEntries:    10000,
	}
	res, err := gate.Reserve(context.Background(), req)
	if err != nil || res.Code != "ACCEPTED" {
		t.Fatalf("reserve: %+v %v", res, err)
	}
	return req
}

func processStreamMessage(t *testing.T, store *mysqlrepo.Store, gate *redisrepo.Gateway, consumer string) redisrepo.Message {
	t.Helper()
	msgs, err := gate.ReadGroup(context.Background(), consumer, 10, time.Millisecond)
	if err != nil || len(msgs) == 0 {
		t.Fatalf("read group: %v len=%d", err, len(msgs))
	}
	wk := worker.New(store, gate, time.Hour)
	if err := wk.Process(context.Background(), msgs[0]); err != nil {
		t.Fatalf("process: %v", err)
	}
	if err := gate.Ack(context.Background(), msgs[0].ID); err != nil {
		t.Fatalf("ack: %v", err)
	}
	return msgs[0]
}

// TestA5RedisUnavailableNewOrderFails verifies SPEC 7.1: a new order must fail
// fast with 503 and must not touch MySQL stock or orders when Redis is down.
func TestA5RedisUnavailableNewOrderFails(t *testing.T) {
	store, gate := setup(t, time.Hour)
	item := createItem(t, store, gate, 10, 1)
	ctx := context.Background()
	before, found, err := gate.GetStock(ctx, item.ID)
	if err != nil || !found {
		t.Fatalf("stock: %d %v %v", before, found, err)
	}

	deadGate := redisrepo.New("127.0.0.1:1", "", 0, gate.Prefix(), "sk:it:dead:stream", "dead-workers", time.Hour)
	defer deadGate.Close()
	svc := service.New(store, deadGate, itConfig())

	_, err = svc.Seckill(ctx, 9001, "a5-redis-down-0001", item.ID, 1)
	assertAppCode(t, err, "SERVICE_UNAVAILABLE")

	after, _, err := gate.GetStock(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("redis stock changed on failed order: %d -> %d", before, after)
	}
	count, err := store.CountOrdersByItem(ctx, item.ID)
	if err != nil || count != 0 {
		t.Fatalf("orders=%d err=%v, want 0", count, err)
	}
}

// TestA5HistoryReturnedWhenRedisUnavailable verifies SPEC 4.2/4.4: historical
// results take priority over dependency failure and are served from MySQL.
func TestA5HistoryReturnedWhenRedisUnavailable(t *testing.T) {
	store, gate := setup(t, time.Hour)
	item := createItem(t, store, gate, 10, 1)
	ctx := context.Background()
	userID := int64(9002)
	key := fmt.Sprintf("a5-history-%d", time.Now().UnixNano())

	if res, err := service.New(store, gate, itConfig()).Seckill(ctx, userID, key, item.ID, 1); err != nil || res.HTTPStatus != 202 {
		t.Fatalf("seckill: %+v %v", res, err)
	}
	processStreamMessage(t, store, gate, "a5-history")

	deadGate := redisrepo.New("127.0.0.1:1", "", 0, gate.Prefix(), "sk:it:dead:stream", "dead-workers", time.Hour)
	defer deadGate.Close()
	svc := service.New(store, deadGate, itConfig())

	replay, err := svc.Seckill(ctx, userID, key, item.ID, 1)
	if err != nil {
		t.Fatalf("history replay: %v", err)
	}
	if replay.HTTPStatus != 200 || replay.Order == nil {
		t.Fatalf("expected historical order, got %+v", replay)
	}
}

// lookupErrorStore simulates MySQL being unavailable before the reservation is
// attempted, without stopping the real server.
type lookupErrorStore struct {
	*mysqlrepo.Store
}

func (s *lookupErrorStore) LookupRequest(context.Context, int64, string) (*model.Order, *model.RequestResult, error) {
	return nil, nil, errors.New("mysql down")
}

// TestA5MySQLUnavailableBeforeReserve verifies SPEC 7.1: a MySQL failure
// detected before pre-deduction returns 503 without touching Redis or MySQL.
func TestA5MySQLUnavailableBeforeReserve(t *testing.T) {
	store, gate := setup(t, time.Hour)
	item := createItem(t, store, gate, 10, 1)
	ctx := context.Background()
	before, found, err := gate.GetStock(ctx, item.ID)
	if err != nil || !found {
		t.Fatalf("stock: %d %v %v", before, found, err)
	}

	svc := service.New(&lookupErrorStore{Store: store}, gate, itConfig())
	_, err = svc.Seckill(ctx, 9003, "a5-mysql-down-0001", item.ID, 1)
	assertAppCode(t, err, "SERVICE_UNAVAILABLE")

	after, _, err := gate.GetStock(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("redis stock changed: %d -> %d", before, after)
	}
	count, err := store.CountOrdersByItem(ctx, item.ID)
	if err != nil || count != 0 {
		t.Fatalf("orders=%d err=%v, want 0", count, err)
	}
}

// TestA5ResultUnknownNotCompensated verifies SPEC 4.4/7.2: an accepted request
// whose Redis idempotency state is lost with no DB order returns RESULT_UNKNOWN
// and is never auto-compensated or marked failed.
func TestA5ResultUnknownNotCompensated(t *testing.T) {
	store, gate := setup(t, time.Hour)
	item := createItem(t, store, gate, 10, 1)
	ctx := context.Background()
	userID := int64(9004)
	key := fmt.Sprintf("a5-unknown-%d", time.Now().UnixNano())
	svc := service.New(store, gate, itConfig())

	if res, err := svc.Seckill(ctx, userID, key, item.ID, 1); err != nil || res.HTTPStatus != 202 {
		t.Fatalf("seckill: %+v %v", res, err)
	}
	reserved, _, err := gate.GetStock(ctx, item.ID)
	if err != nil || reserved != 9 {
		t.Fatalf("redis stock after reserve=%d err=%v, want 9", reserved, err)
	}

	status, err := svc.GetRequestStatus(ctx, userID, key)
	if err != nil || status.HTTPStatus != 202 {
		t.Fatalf("expected PROCESSING, got %+v %v", status, err)
	}

	if err := gate.Client().Del(ctx, gate.ReqKey(userID, key)).Err(); err != nil {
		t.Fatal(err)
	}
	_, err = svc.GetRequestStatus(ctx, userID, key)
	assertAppCode(t, err, "RESULT_UNKNOWN")

	after, _, err := gate.GetStock(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after != reserved {
		t.Fatalf("result-unknown must not compensate: stock %d -> %d", reserved, after)
	}
	count, err := store.CountOrdersByItem(ctx, item.ID)
	if err != nil || count != 0 {
		t.Fatalf("orders=%d err=%v, want 0", count, err)
	}
}

// TestA2ReplayFromDBAfterRedisKeyDeleted verifies SPEC A2: after the Redis
// idempotency key (and even stock) is gone, replay returns the MySQL order.
func TestA2ReplayFromDBAfterRedisKeyDeleted(t *testing.T) {
	store, gate := setup(t, time.Hour)
	item := createItem(t, store, gate, 10, 1)
	ctx := context.Background()
	userID := int64(9010)
	key := fmt.Sprintf("a2-deleted-%d", time.Now().UnixNano())
	svc := service.New(store, gate, itConfig())

	if res, err := svc.Seckill(ctx, userID, key, item.ID, 1); err != nil || res.HTTPStatus != 202 {
		t.Fatalf("seckill: %+v %v", res, err)
	}
	processStreamMessage(t, store, gate, "a2-deleted")

	status, err := svc.GetRequestStatus(ctx, userID, key)
	if err != nil || status.Order == nil {
		t.Fatalf("status: %+v %v", status, err)
	}
	orderNo := status.Order.OrderNo

	if err := gate.Client().Del(ctx, gate.ReqKey(userID, key)).Err(); err != nil {
		t.Fatal(err)
	}
	if err := gate.Client().Set(ctx, gate.StockKey(item.ID), 0, 0).Err(); err != nil {
		t.Fatal(err)
	}

	replay, err := svc.Seckill(ctx, userID, key, item.ID, 1)
	if err != nil {
		t.Fatalf("replay after key deletion: %v", err)
	}
	if replay.Order == nil || replay.Order.OrderNo != orderNo {
		t.Fatalf("replay returned %+v, want order %s", replay, orderNo)
	}
}

// TestA2RestartDoesNotOverwriteInFlightReservation verifies SPEC 4.1/10: startup
// initialization must not reset stock that already has in-flight reservations.
func TestA2RestartDoesNotOverwriteInFlightReservation(t *testing.T) {
	store, gate := setup(t, time.Hour)
	item := createItem(t, store, gate, 10, 1)
	ctx := context.Background()
	svc := service.New(store, gate, itConfig())

	if res, err := svc.Seckill(ctx, 9011, "a2-inflight-0001", item.ID, 1); err != nil || res.HTTPStatus != 202 {
		t.Fatalf("seckill: %+v %v", res, err)
	}
	if err := gate.SetStockNX(ctx, item.ID, item.TotalStock); err == nil {
		t.Fatal("SET NX must not overwrite an existing stock key")
	}
	if err := gate.EnsureGroup(ctx); err != nil {
		t.Fatalf("ensure group: %v", err)
	}
	stock, _, err := gate.GetStock(ctx, item.ID)
	if err != nil || stock != 9 {
		t.Fatalf("stock after restart-like calls=%d err=%v, want 9", stock, err)
	}
}

// TestA3RestockMarkerMissingDoesNotDoubleRestock verifies SPEC A3/6.4: a missing
// (expired) restock marker must not trigger a second Redis restock.
func TestA3RestockMarkerMissingDoesNotDoubleRestock(t *testing.T) {
	store, gate := setup(t, time.Millisecond)
	drainExpiredOrders(t, store, gate)
	item := createItem(t, store, gate, 1, 1)
	ctx := context.Background()
	req := reserveOne(t, gate, item, 7002, fmt.Sprintf("a3-marker-%d", time.Now().UnixNano()), 1)
	out, err := store.HandleReservation(ctx, mysqlrepo.ReservationInput{
		ReservationID: req.ReservationID, UserID: req.UserID, IdempotencyKey: req.Key,
		ItemID: req.ItemID, Quantity: req.Quantity, PayloadHash: req.PayloadHash,
	})
	if err != nil || out.Order == nil {
		t.Fatalf("handle: %+v %v", out, err)
	}
	time.Sleep(20 * time.Millisecond)
	svc := service.New(store, gate, config.Config{CancelBatch: 10})
	if n, err := svc.CancelExpired(ctx); err != nil || n != 1 {
		t.Fatalf("first cancel n=%d err=%v, want 1 after draining", n, err)
	}
	marker := gate.Prefix() + "restocked:" + out.Order.OrderNo
	if exists, _ := gate.Client().Exists(ctx, marker).Result(); exists != 1 {
		t.Fatal("restock marker missing after cancel")
	}
	// Simulate the 24h marker expiring (or being lost): the order is already
	// CANCELLED, so no further restock may happen. Other unrelated expired
	// orders may be picked up here, so assert on this order/stock rather than n.
	if err := gate.Client().Del(ctx, marker).Err(); err != nil {
		t.Fatal(err)
	}
	if n, err := svc.CancelExpired(ctx); err != nil || n != 0 {
		t.Fatalf("second cancel n=%d err=%v, want 0 after draining", n, err)
	}
	stock, _, err := gate.GetStock(ctx, item.ID)
	if err != nil || stock != 1 {
		t.Fatalf("redis stock=%d err=%v, want 1 (no double restock)", stock, err)
	}
	logs, err := store.CountStockLogs(ctx, out.Order.OrderNo, "CANCEL")
	if err != nil || logs != 1 {
		t.Fatalf("cancel logs=%d err=%v, want 1", logs, err)
	}
}

// TestA4RedeliveryAfterCommitDoesNotRestockRedis verifies SPEC A4: a committed
// order that is delivered again must not add orders or increase Redis stock.
func TestA4RedeliveryAfterCommitDoesNotRestockRedis(t *testing.T) {
	store, gate := setup(t, time.Hour)
	item := createItem(t, store, gate, 1, 1)
	ctx := context.Background()
	reserveOne(t, gate, item, 7003, fmt.Sprintf("a4-commit-%d", time.Now().UnixNano()), 1)

	msgs, err := gate.ReadGroup(ctx, "a4-commit", 10, time.Millisecond)
	if err != nil || len(msgs) == 0 {
		t.Fatalf("read: %v len=%d", err, len(msgs))
	}
	wk := worker.New(store, gate, time.Hour)
	if err := wk.Process(ctx, msgs[0]); err != nil {
		t.Fatalf("process: %v", err)
	}
	// Simulate a crash before XACK, then redeliver the same message.
	if err := wk.Process(ctx, msgs[0]); err != nil {
		t.Fatalf("redeliver: %v", err)
	}
	stock, _, err := gate.GetStock(ctx, item.ID)
	if err != nil || stock != 0 {
		t.Fatalf("redis stock=%d err=%v, want 0 (no restock on redelivery)", stock, err)
	}
	count, err := store.CountOrdersByItem(ctx, item.ID)
	if err != nil || count != 1 {
		t.Fatalf("orders=%d err=%v, want 1", count, err)
	}
}

// TestA4CompensationCrashWindows verifies SPEC A4/6.4: compensation that
// succeeded in Redis but did not persist DONE is completed on redelivery exactly
// once; after DONE a missing marker is ignored.
func TestA4CompensationCrashWindows(t *testing.T) {
	store, gate := setup(t, time.Hour)
	item := createItem(t, store, gate, 10, 1)
	ctx := context.Background()
	req := reserveOne(t, gate, item, 7004, fmt.Sprintf("a4-crash-%d", time.Now().UnixNano()), 3)

	out, err := store.HandleReservation(ctx, mysqlrepo.ReservationInput{
		ReservationID: req.ReservationID, UserID: req.UserID, IdempotencyKey: req.Key,
		ItemID: req.ItemID, Quantity: req.Quantity, PayloadHash: req.PayloadHash,
	})
	if err != nil || out.Order != nil || !out.Compensate {
		t.Fatalf("expected rejected reservation: %+v %v", out, err)
	}
	// First compensation succeeds in Redis but the crash happens before DONE is
	// written to MySQL.
	code, err := gate.Compensate(ctx, item.ID, req.UserID, req.Key, req.ReservationID, req.Quantity, out.Reason)
	if err != nil || code != "OK" {
		t.Fatalf("compensate: %s %v", code, err)
	}
	rr, err := store.GetReservationResult(ctx, req.ReservationID)
	if err != nil || rr.CompensationState != "PENDING" {
		t.Fatalf("reservation state=%+v err=%v, want PENDING", rr, err)
	}

	msg := redisrepo.Message{
		ID: "a4-crash-msg", ReservationID: req.ReservationID, IdempotencyKey: req.Key,
		UserID: req.UserID, ItemID: req.ItemID, Quantity: req.Quantity, PayloadHash: req.PayloadHash,
	}
	wk := worker.New(store, gate, time.Hour)
	if err := wk.Process(ctx, msg); err != nil {
		t.Fatalf("redelivery after redis compensation: %v", err)
	}
	stock, _, err := gate.GetStock(ctx, item.ID)
	if err != nil || stock != 10 {
		t.Fatalf("redis stock=%d err=%v, want 10 (compensated once)", stock, err)
	}
	rr, err = store.GetReservationResult(ctx, req.ReservationID)
	if err != nil || rr.CompensationState != "DONE" {
		t.Fatalf("reservation state=%+v err=%v, want DONE", rr, err)
	}

	// DONE is durable; even if the Redis marker disappears, redelivery only ACKs.
	if err := gate.Client().Del(ctx, gate.Prefix()+"compensated:"+req.ReservationID).Err(); err != nil {
		t.Fatal(err)
	}
	if err := wk.Process(ctx, msg); err != nil {
		t.Fatalf("redelivery after DONE: %v", err)
	}
	stock, _, err = gate.GetStock(ctx, item.ID)
	if err != nil || stock != 10 {
		t.Fatalf("redis stock=%d err=%v, want 10 after DONE", stock, err)
	}
}

// TestExtraReservationCompensatedOnce verifies SPEC 4.3: two different
// reservationIds for the same business key produce one order and the extra
// pre-deduction is compensated exactly once.
func TestExtraReservationCompensatedOnce(t *testing.T) {
	store, gate := setup(t, time.Hour)
	item := createItem(t, store, gate, 10, 1)
	ctx := context.Background()
	userID := int64(7010)
	key := fmt.Sprintf("extra-%d", time.Now().UnixNano())
	payload := service.PayloadHash(item.ID, 1)

	// Simulate Redis idempotency loss: both reservations reached the stream and
	// already decremented Redis stock.
	if err := gate.Client().DecrBy(ctx, gate.StockKey(item.ID), 2).Err(); err != nil {
		t.Fatal(err)
	}
	first := mysqlrepo.ReservationInput{
		ReservationID: fmt.Sprintf("r-extra-a-%d", time.Now().UnixNano()), UserID: userID,
		IdempotencyKey: key, ItemID: item.ID, Quantity: 1, PayloadHash: payload,
	}
	second := mysqlrepo.ReservationInput{
		ReservationID: fmt.Sprintf("r-extra-b-%d", time.Now().UnixNano()), UserID: userID,
		IdempotencyKey: key, ItemID: item.ID, Quantity: 1, PayloadHash: payload,
	}

	out1, err := store.HandleReservation(ctx, first)
	if err != nil || out1.Order == nil {
		t.Fatalf("first reservation: %+v %v", out1, err)
	}
	out2, err := store.HandleReservation(ctx, second)
	if err != nil || out2.Order != nil || !out2.Compensate || out2.Kind != "EXTRA_RESERVATION" {
		t.Fatalf("second reservation must be extra: %+v %v", out2, err)
	}

	order, req, err := store.LookupRequest(ctx, userID, key)
	if err != nil || order == nil || req == nil {
		t.Fatalf("lookup: %+v %+v %v", order, req, err)
	}
	if req.State != "SUCCEEDED" || req.WinningReservationID != first.ReservationID {
		t.Fatalf("request result=%+v, want SUCCEEDED by %s", req, first.ReservationID)
	}

	msg := redisrepo.Message{
		ID: "extra-msg", ReservationID: second.ReservationID, IdempotencyKey: key,
		UserID: userID, ItemID: item.ID, Quantity: 1, PayloadHash: payload,
	}
	wk := worker.New(store, gate, time.Hour)
	if err := wk.Process(ctx, msg); err != nil {
		t.Fatalf("compensate extra: %v", err)
	}
	if err := wk.Process(ctx, msg); err != nil {
		t.Fatalf("duplicate extra: %v", err)
	}

	stock, _, err := gate.GetStock(ctx, item.ID)
	if err != nil || stock != 9 {
		t.Fatalf("redis stock=%d err=%v, want 9", stock, err)
	}
	count, err := store.CountOrdersByItem(ctx, item.ID)
	if err != nil || count != 1 {
		t.Fatalf("orders=%d err=%v, want 1", count, err)
	}
	rr, err := store.GetReservationResult(ctx, second.ReservationID)
	if err != nil || rr.Outcome != "EXTRA_RESERVATION" || rr.CompensationState != "DONE" {
		t.Fatalf("reservation result=%+v err=%v", rr, err)
	}
}

// TestA7GlobalRateLimitAppliesToReplay verifies SPEC A7/2.2: the ingress global
// limiter runs before the idempotency lookup, so replays can be rate limited.
func TestA7GlobalRateLimitAppliesToReplay(t *testing.T) {
	store, gate := setup(t, time.Hour)
	item := createItem(t, store, gate, 10, 1)
	ctx := context.Background()
	cfg := itConfig()
	cfg.IngressRPS = 0.01
	cfg.IngressBurst = 1
	svc := service.New(store, gate, cfg)
	userID := int64(7020)
	key := fmt.Sprintf("a7-rate-%d", time.Now().UnixNano())

	if res, err := svc.Seckill(ctx, userID, key, item.ID, 1); err != nil || res.HTTPStatus != 202 {
		t.Fatalf("first request: %+v %v", res, err)
	}
	reserved, _, err := gate.GetStock(ctx, item.ID)
	if err != nil || reserved != 9 {
		t.Fatalf("stock after first=%d err=%v, want 9", reserved, err)
	}
	_, err = svc.Seckill(ctx, userID, key, item.ID, 1)
	assertAppCode(t, err, "RATE_LIMITED")
	after, _, err := gate.GetStock(ctx, item.ID)
	if err != nil || after != reserved {
		t.Fatalf("stock changed on limited replay: %d -> %d", reserved, after)
	}
}

// TestA7QueueFullDoesNotDeduct verifies SPEC A7/4.2: exhausting the stream
// capacity rejects new requests with QUEUE_FULL without deducting stock.
func TestA7QueueFullDoesNotDeduct(t *testing.T) {
	store, gate := setup(t, time.Hour)
	item := createItem(t, store, gate, 10, 1)
	ctx := context.Background()
	cfg := itConfig()
	cfg.StreamMax = 1
	svc := service.New(store, gate, cfg)

	if res, err := svc.Seckill(ctx, 7030, fmt.Sprintf("a7-queue-a-%d", time.Now().UnixNano()), item.ID, 1); err != nil || res.HTTPStatus != 202 {
		t.Fatalf("first request: %+v %v", res, err)
	}
	_, err := svc.Seckill(ctx, 7031, fmt.Sprintf("a7-queue-b-%d", time.Now().UnixNano()), item.ID, 1)
	assertAppCode(t, err, "QUEUE_FULL")
	stock, _, err := gate.GetStock(ctx, item.ID)
	if err != nil || stock != 9 {
		t.Fatalf("stock=%d err=%v, want 9", stock, err)
	}
	length, err := gate.StreamLen(ctx)
	if err != nil || length != 1 {
		t.Fatalf("stream length=%d err=%v, want 1", length, err)
	}
}

// TestPaymentRaceReturnsPayAfterCancel deterministically forces the window
// where MarkPaid has read PENDING_PAYMENT but a concurrent cancellation commits
// before its conditional update. The loser must report PAY_AFTER_CANCEL
// (SPEC 4.5/5.4), not SERVICE_UNAVAILABLE from a stale snapshot re-read.
func TestPaymentRaceReturnsPayAfterCancel(t *testing.T) {
	store, gate := setup(t, time.Hour)
	item := createItem(t, store, gate, 1, 1)
	ctx := context.Background()
	req := reserveOne(t, gate, item, 7041, fmt.Sprintf("a7-payrace-%d", time.Now().UnixNano()), 1)
	out, err := store.HandleReservation(ctx, mysqlrepo.ReservationInput{
		ReservationID: req.ReservationID, UserID: req.UserID, IdempotencyKey: req.Key,
		ItemID: req.ItemID, Quantity: req.Quantity, PayloadHash: req.PayloadHash,
	})
	if err != nil || out.Order == nil {
		t.Fatalf("handle: %+v %v", out, err)
	}
	orderNo := out.Order.OrderNo

	dsn := os.Getenv("SECKILL_IT_DSN")
	raw, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("open raw sql: %v", err)
	}
	defer raw.Close()

	// Hold the order row lock with an uncommitted cancellation.
	tx, err := raw.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin raw tx: %v", err)
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE orders SET status = 'CANCELLED', cancelled_at = CURRENT_TIMESTAMP(3)
		 WHERE order_no = ? AND status = 'PENDING_PAYMENT'`, orderNo)
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("raw cancel: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		_ = tx.Rollback()
		t.Fatalf("raw cancel affected %d rows", n)
	}

	payErrCh := make(chan error, 1)
	go func() {
		_, err := store.MarkPaid(context.Background(), req.UserID, orderNo, "txn-pay-race", out.Order.AmountCents)
		payErrCh <- err
	}()

	// Let MarkPaid read PENDING_PAYMENT and block on the conditional update.
	time.Sleep(300 * time.Millisecond)
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit raw cancel: %v", err)
	}

	payErr := <-payErrCh
	if !errors.Is(payErr, mysqlrepo.ErrPayAfterCancel) {
		t.Fatalf("expected PAY_AFTER_CANCEL, got %v", payErr)
	}
}

// TestPayCancelRaceOnlyOneWins verifies SPEC 4.5/5.4: payment and cancellation
// compete via conditional updates and exactly one terminal state wins.
func TestPayCancelRaceOnlyOneWins(t *testing.T) {
	store, gate := setup(t, time.Millisecond)
	drainExpiredOrders(t, store, gate)
	item := createItem(t, store, gate, 1, 1)
	ctx := context.Background()
	req := reserveOne(t, gate, item, 7040, fmt.Sprintf("a7-race-%d", time.Now().UnixNano()), 1)
	out, err := store.HandleReservation(ctx, mysqlrepo.ReservationInput{
		ReservationID: req.ReservationID, UserID: req.UserID, IdempotencyKey: req.Key,
		ItemID: req.ItemID, Quantity: req.Quantity, PayloadHash: req.PayloadHash,
	})
	if err != nil || out.Order == nil {
		t.Fatalf("handle: %+v %v", out, err)
	}
	orderNo := out.Order.OrderNo
	time.Sleep(20 * time.Millisecond)

	svc := service.New(store, gate, config.Config{CancelBatch: 10})
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, _ = store.MarkPaid(context.Background(), req.UserID, orderNo, "txn-race", out.Order.AmountCents)
	}()
	go func() {
		defer wg.Done()
		<-start
		_, _ = svc.CancelExpired(context.Background())
	}()
	close(start)
	wg.Wait()

	final, err := store.GetOrderByNo(ctx, orderNo, req.UserID)
	if err != nil {
		t.Fatalf("final order: %v", err)
	}
	dbItem, err := store.GetItem(ctx, item.ID)
	if err != nil {
		t.Fatalf("item: %v", err)
	}
	logs, err := store.CountStockLogs(ctx, orderNo, "CANCEL")
	if err != nil {
		t.Fatalf("cancel logs: %v", err)
	}
	switch final.Status {
	case "PAID":
		if dbItem.AvailableStock != 0 || logs != 0 {
			t.Fatalf("paid order must not be restocked: stock=%d logs=%d", dbItem.AvailableStock, logs)
		}
	case "CANCELLED":
		if dbItem.AvailableStock != 1 || logs != 1 {
			t.Fatalf("cancelled order must restock once: stock=%d logs=%d", dbItem.AvailableStock, logs)
		}
	default:
		t.Fatalf("unexpected terminal status %q", final.Status)
	}
}

// TestCompensationPartialExecutionDoesNotMarkDone ensures a compensation whose
// INCRBY cannot succeed (invalid stock value) does not leave a marker that a
// retry would treat as ALREADY and record DONE.
func TestCompensationPartialExecutionDoesNotMarkDone(t *testing.T) {
	store, gate := setup(t, time.Hour)
	item := createItem(t, store, gate, 10, 1)
	ctx := context.Background()
	req := reserveOne(t, gate, item, 7060, fmt.Sprintf("partial-%d", time.Now().UnixNano()), 3)
	out, err := store.HandleReservation(ctx, mysqlrepo.ReservationInput{
		ReservationID: req.ReservationID, UserID: req.UserID, IdempotencyKey: req.Key,
		ItemID: req.ItemID, Quantity: req.Quantity, PayloadHash: req.PayloadHash,
	})
	if err != nil || out.Order != nil || !out.Compensate {
		t.Fatalf("expected rejected reservation: %+v %v", out, err)
	}
	if err := gate.Client().Set(ctx, gate.StockKey(item.ID), "not-a-number", 0).Err(); err != nil {
		t.Fatal(err)
	}

	wk := worker.New(store, gate, time.Hour)
	msg := redisrepo.Message{
		ID: "partial-msg", ReservationID: req.ReservationID, IdempotencyKey: req.Key,
		UserID: req.UserID, ItemID: req.ItemID, Quantity: req.Quantity, PayloadHash: req.PayloadHash,
	}
	if err := wk.Process(ctx, msg); err == nil {
		t.Fatal("expected BAD_STOCK compensation failure")
	}
	rr, err := store.GetReservationResult(ctx, req.ReservationID)
	if err != nil || rr.CompensationState != "PENDING" {
		t.Fatalf("reservation state=%+v err=%v, want PENDING", rr, err)
	}
	if err := wk.Process(ctx, msg); err == nil {
		t.Fatal("retry must not be treated as ALREADY")
	}
	rr, err = store.GetReservationResult(ctx, req.ReservationID)
	if err != nil || rr.CompensationState != "PENDING" {
		t.Fatalf("retry marked DONE without restoring stock: %+v err=%v", rr, err)
	}
	if exists, _ := gate.Client().Exists(ctx, gate.Prefix()+"compensated:"+req.ReservationID).Result(); exists != 0 {
		t.Fatal("compensation marker must not exist after partial execution")
	}
}

// failOnceAfterHandleStore delegates to the real store (so the order commits)
// but reports the first result as unknown, simulating a lost commit response.
type failOnceAfterHandleStore struct {
	*mysqlrepo.Store
	calls int
}

func (s *failOnceAfterHandleStore) HandleReservation(ctx context.Context, in mysqlrepo.ReservationInput) (mysqlrepo.ReservationOutcome, error) {
	s.calls++
	out, err := s.Store.HandleReservation(ctx, in)
	if s.calls == 1 {
		return mysqlrepo.ReservationOutcome{}, errors.New("simulated unknown commit result")
	}
	return out, err
}

// TestUnknownCommitResultRecoversUnique verifies SPEC 7.2: an unknown commit
// must not be compensated or treated as failed; retry observes the durable
// order and does not create a second one or increase Redis stock.
func TestUnknownCommitResultRecoversUnique(t *testing.T) {
	store, gate := setup(t, time.Hour)
	item := createItem(t, store, gate, 1, 1)
	ctx := context.Background()
	req := reserveOne(t, gate, item, 7061, fmt.Sprintf("unknown-%d", time.Now().UnixNano()), 1)
	wrapped := &failOnceAfterHandleStore{Store: store}
	wk := worker.New(wrapped, gate, time.Hour)
	msg := redisrepo.Message{
		ID: "unknown-msg", ReservationID: req.ReservationID, IdempotencyKey: req.Key,
		UserID: req.UserID, ItemID: req.ItemID, Quantity: req.Quantity, PayloadHash: req.PayloadHash,
	}

	if err := wk.Process(ctx, msg); err == nil {
		t.Fatal("expected unknown commit result to keep pending")
	}
	if wrapped.calls != 1 {
		t.Fatalf("handle calls=%d, want 1", wrapped.calls)
	}
	count, err := store.CountOrdersByItem(ctx, item.ID)
	if err != nil || count != 1 {
		t.Fatalf("committed order must persist: count=%d err=%v", count, err)
	}
	if stock, _, err := gate.GetStock(ctx, item.ID); err != nil || stock != 0 {
		t.Fatalf("redis stock=%d err=%v, want 0 (no blind restock)", stock, err)
	}

	if err := wk.Process(ctx, msg); err != nil {
		t.Fatalf("retry after recovery: %v", err)
	}
	if wrapped.calls != 2 {
		t.Fatalf("handle calls=%d, want 2", wrapped.calls)
	}
	count, err = store.CountOrdersByItem(ctx, item.ID)
	if err != nil || count != 1 {
		t.Fatalf("retry must not create a second order: count=%d err=%v", count, err)
	}
	if stock, _, err := gate.GetStock(ctx, item.ID); err != nil || stock != 0 {
		t.Fatalf("retry restocked redis: stock=%d err=%v", stock, err)
	}
	order, reqResult, err := store.LookupRequest(ctx, req.UserID, req.Key)
	if err != nil || order == nil || reqResult == nil || reqResult.State != "SUCCEEDED" {
		t.Fatalf("durable result: order=%+v request=%+v err=%v", order, reqResult, err)
	}
}
