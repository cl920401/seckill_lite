package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"seckill_lite/internal/config"
	"seckill_lite/internal/model"
	"seckill_lite/internal/mysqlrepo"
	"seckill_lite/internal/redisrepo"
)

type fakeStore struct {
	mu        sync.Mutex
	order     *model.Order
	request   *model.RequestResult
	item      *model.Item
	err       error
	lookupErr error
	itemErr   error
	orderErr  error
	payErr    error
	payResult *model.Order
	cancelErr error
	cancelled []model.CancelledOrder
	activity  *model.Activity
	items     []model.Item
	createErr error
	listErr   error
	orders    []model.Order
	lookups   int
}

func (f *fakeStore) CreateActivity(ctx context.Context, in model.ActivityInput) (*model.Activity, []model.Item, error) {
	if f.createErr != nil {
		return nil, nil, f.createErr
	}
	return f.activity, f.items, nil
}
func (f *fakeStore) GetItem(ctx context.Context, itemID int64) (*model.Item, error) {
	if f.itemErr != nil {
		return nil, f.itemErr
	}
	if f.err != nil {
		return nil, f.err
	}
	if f.item == nil {
		return nil, mysqlrepo.ErrItemNotFound
	}
	return f.item, nil
}
func (f *fakeStore) GetActivityStock(ctx context.Context, id int64) (*model.Activity, []model.Item, error) {
	if f.err != nil {
		return nil, nil, f.err
	}
	if f.activity == nil {
		return nil, nil, mysqlrepo.ErrNotFound
	}
	return f.activity, f.items, nil
}
func (f *fakeStore) LookupRequest(ctx context.Context, userID int64, key string) (*model.Order, *model.RequestResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lookups++
	if f.lookupErr != nil {
		return nil, nil, f.lookupErr
	}
	if f.err != nil {
		return nil, nil, f.err
	}
	return f.order, f.request, nil
}
func (f *fakeStore) GetOrderByNo(ctx context.Context, orderNo string, userID int64) (*model.Order, error) {
	if f.orderErr != nil {
		return nil, f.orderErr
	}
	if f.order == nil {
		return nil, mysqlrepo.ErrNotFound
	}
	return f.order, nil
}
func (f *fakeStore) ListOrders(ctx context.Context, userID int64, limit int, beforeID int64) ([]model.Order, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	if f.err != nil {
		return nil, f.err
	}
	return f.orders, nil
}
func (f *fakeStore) MarkPaid(ctx context.Context, userID int64, orderNo, txn string, amount int64) (*model.Order, error) {
	if f.payErr != nil {
		return nil, f.payErr
	}
	if f.err != nil {
		return nil, f.err
	}
	return f.payResult, nil
}
func (f *fakeStore) CancelExpired(ctx context.Context, limit int) ([]model.CancelledOrder, error) {
	if f.cancelErr != nil {
		return nil, f.cancelErr
	}
	return f.cancelled, f.err
}

type fakeGate struct {
	mu            sync.Mutex
	stock         int64
	states        map[string]redisrepo.RequestState
	item          *model.Item
	allow         bool
	denyUser      bool
	allowErr      error
	getRequestErr error
	reserveErr    error
	reserveCode   string
	snapshotErr   error
	setStockErr   error
	restockCode   string
	restockErr    error
	restockCalls  int
	reserveCalls  int
}

func newFakeGate(stock int64, item model.Item) *fakeGate {
	return &fakeGate{stock: stock, states: make(map[string]redisrepo.RequestState), item: &item, allow: true}
}

func (f *fakeGate) Allow(ctx context.Context, key string, rate float64, burst, cost int) (bool, int, error) {
	if f.allowErr != nil {
		return false, 0, f.allowErr
	}
	if f.denyUser && key == "user" {
		return false, 0, nil
	}
	return f.allow, 0, nil
}
func (f *fakeGate) GetRequest(ctx context.Context, userID int64, key string) (*redisrepo.RequestState, error) {
	if f.getRequestErr != nil {
		return nil, f.getRequestErr
	}
	if f.allowErr != nil {
		return nil, f.allowErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	state, ok := f.states[stateKey(userID, key)]
	if !ok {
		return nil, nil
	}
	copy := state
	return &copy, nil
}
func (f *fakeGate) Reserve(ctx context.Context, req redisrepo.ReserveRequest) (redisrepo.ReserveResult, error) {
	if f.reserveErr != nil {
		return redisrepo.ReserveResult{}, f.reserveErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reserveCalls++
	if f.reserveCode != "" {
		return redisrepo.ReserveResult{Code: f.reserveCode}, nil
	}
	k := stateKey(req.UserID, req.Key)
	if state, ok := f.states[k]; ok {
		if state.PayloadHash != req.PayloadHash {
			return redisrepo.ReserveResult{Code: "CONFLICT"}, nil
		}
		return redisrepo.ReserveResult{Code: "DUPLICATE", Status: state.Status, OrderNo: state.OrderNo}, nil
	}
	if f.stock < int64(req.Quantity) {
		return redisrepo.ReserveResult{Code: "SOLD_OUT"}, nil
	}
	f.stock -= int64(req.Quantity)
	f.states[k] = redisrepo.RequestState{
		Status: "PROCESSING", ReservationID: req.ReservationID,
		PayloadHash: req.PayloadHash,
	}
	return redisrepo.ReserveResult{Code: "ACCEPTED"}, nil
}
func (f *fakeGate) MarkSuccess(ctx context.Context, userID int64, key, reservationID, orderNo string) error {
	return nil
}
func (f *fakeGate) Compensate(ctx context.Context, itemID int64, userID int64, key, reservationID string, quantity int, reason string) (string, error) {
	return "OK", nil
}
func (f *fakeGate) Restock(ctx context.Context, itemID int64, orderNo string, quantity int) (string, error) {
	f.mu.Lock()
	f.restockCalls++
	f.mu.Unlock()
	if f.restockErr != nil {
		return "", f.restockErr
	}
	if f.restockCode != "" {
		return f.restockCode, nil
	}
	return "OK", nil
}
func (f *fakeGate) GetStock(ctx context.Context, itemID int64) (int64, bool, error) {
	return f.stock, true, nil
}
func (f *fakeGate) SetStockNX(ctx context.Context, itemID int64, stock int) error {
	return f.setStockErr
}
func (f *fakeGate) SetItemSnapshot(ctx context.Context, item model.Item, ttl time.Duration) error {
	return f.snapshotErr
}
func (f *fakeGate) GetItemSnapshot(ctx context.Context, itemID int64) (*model.Item, bool, error) {
	if f.snapshotErr != nil {
		return nil, false, f.snapshotErr
	}
	if f.item == nil {
		return nil, false, nil
	}
	copy := *f.item
	return &copy, true, nil
}
func (f *fakeGate) IngressKey() string          { return "ingress" }
func (f *fakeGate) QueryKey() string            { return "query" }
func (f *fakeGate) UserKey(userID int64) string { return "user" }

func stateKey(userID int64, key string) string {
	return string(rune(userID)) + "|" + key
}

func testConfig() config.Config {
	return config.Config{
		IngressRPS: 1000, IngressBurst: 1000,
		UserRPS: 1000, UserBurst: 1000,
		QueryRPS: 1000, QueryBurst: 1000,
		RequestTTL: time.Hour, StreamMax: 1000,
		CancelBatch: 10, HistoryLookups: 4,
	}
}

func testItem() model.Item {
	now := time.Now().UTC()
	return model.Item{
		ID: 1, ActivityID: 1, PriceCents: 100,
		TotalStock: 10, AvailableStock: 10, PerUserLimit: 2,
		ActivityStart: now.Add(-time.Hour), ActivityEnd: now.Add(time.Hour),
		ActivityStatus: "ACTIVE",
	}
}

func TestConcurrentSameKeyDeductsOnce(t *testing.T) {
	item := testItem()
	gate := newFakeGate(10, item)
	store := &fakeStore{item: &item}
	svc := New(store, gate, testConfig())
	key := "same-key-1234"

	const workers = 50
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := svc.Seckill(context.Background(), 7, key, item.ID, 1)
			if err != nil {
				errs <- err
				return
			}
			if res.HTTPStatus != 202 {
				errs <- errors.New("expected processing response")
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent seckill failed: %v", err)
	}
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if gate.stock != 9 {
		t.Fatalf("stock = %d, want 9", gate.stock)
	}
	if len(gate.states) != 1 {
		t.Fatalf("states = %d, want 1", len(gate.states))
	}
}

func TestGlobalRateLimitRejectsWithoutReserve(t *testing.T) {
	item := testItem()
	gate := newFakeGate(10, item)
	gate.allow = false
	store := &fakeStore{item: &item}
	svc := New(store, gate, testConfig())

	_, err := svc.Seckill(context.Background(), 1, "rate-limit-key", item.ID, 1)
	if err == nil {
		t.Fatal("expected rate limit error")
	}
	appErr, ok := AsAppError(err)
	if !ok || appErr.Code != "RATE_LIMITED" {
		t.Fatalf("error = %v, want RATE_LIMITED", err)
	}
	if gate.reserveCalls != 0 {
		t.Fatalf("reserve calls = %d, want 0", gate.reserveCalls)
	}
}

func TestReplayReturnsHistory(t *testing.T) {
	item := testItem()
	hash := PayloadHash(item.ID, 1)
	order := &model.Order{
		OrderNo: "SK1", UserID: 1, ItemID: item.ID, Quantity: 1,
		PayloadHash: hash, Status: "PENDING_PAYMENT",
	}
	gate := newFakeGate(10, item)
	store := &fakeStore{item: &item, order: order}
	svc := New(store, gate, testConfig())

	res, err := svc.Seckill(context.Background(), 1, "replay-key-123", item.ID, 1)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if res.HTTPStatus != 200 || res.Order == nil || res.Order.OrderNo != "SK1" {
		t.Fatalf("unexpected replay result: %+v", res)
	}
	if gate.reserveCalls != 0 {
		t.Fatalf("reserve calls = %d, want 0", gate.reserveCalls)
	}
}

func TestRedisFailureStillReturnsHistory(t *testing.T) {
	item := testItem()
	hash := PayloadHash(item.ID, 1)
	order := &model.Order{
		OrderNo: "SK2", UserID: 1, ItemID: item.ID, Quantity: 1,
		PayloadHash: hash, Status: "PENDING_PAYMENT",
	}
	gate := newFakeGate(10, item)
	gate.allowErr = errors.New("redis down")
	store := &fakeStore{item: &item, order: order}
	svc := New(store, gate, testConfig())

	res, err := svc.Seckill(context.Background(), 1, "redis-down-key", item.ID, 1)
	if err != nil {
		t.Fatalf("history fallback: %v", err)
	}
	if res.Order == nil || res.Order.OrderNo != "SK2" {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestSoldOut(t *testing.T) {
	item := testItem()
	gate := newFakeGate(0, item)
	store := &fakeStore{item: &item}
	svc := New(store, gate, testConfig())

	_, err := svc.Seckill(context.Background(), 1, "sold-out-key-1", item.ID, 1)
	appErr, ok := AsAppError(err)
	if !ok || appErr.Code != "SOLD_OUT" {
		t.Fatalf("error = %v, want SOLD_OUT", err)
	}
}

func TestActivityWindowErrors(t *testing.T) {
	item := testItem()
	item.ActivityStart = time.Now().UTC().Add(time.Hour)
	gate := newFakeGate(10, item)
	store := &fakeStore{item: &item}
	svc := New(store, gate, testConfig())
	_, err := svc.Seckill(context.Background(), 1, "not-started-key", item.ID, 1)
	appErr, ok := AsAppError(err)
	if !ok || appErr.Code != "ACTIVITY_NOT_STARTED" {
		t.Fatalf("error = %v, want ACTIVITY_NOT_STARTED", err)
	}

	item = testItem()
	item.ActivityEnd = time.Now().UTC().Add(-time.Hour)
	gate = newFakeGate(10, item)
	store = &fakeStore{item: &item}
	svc = New(store, gate, testConfig())
	_, err = svc.Seckill(context.Background(), 1, "ended-key-123", item.ID, 1)
	appErr, ok = AsAppError(err)
	if !ok || appErr.Code != "ACTIVITY_ENDED" {
		t.Fatalf("error = %v, want ACTIVITY_ENDED", err)
	}
}

func TestPayloadHashAndValidation(t *testing.T) {
	if PayloadHash(1, 2) == PayloadHash(2, 1) {
		t.Fatal("payload hash should bind item and quantity")
	}
	if len(RequestID(1, "key")) == 0 {
		t.Fatal("request id must not be empty")
	}
}
