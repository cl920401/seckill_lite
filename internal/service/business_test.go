package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"seckill_lite/internal/model"
	"seckill_lite/internal/mysqlrepo"
	"seckill_lite/internal/redisrepo"
)

func TestReplayRejectedAndConflict(t *testing.T) {
	item := testItem()
	hash := PayloadHash(item.ID, 1)
	store := &fakeStore{item: &item, request: &model.RequestResult{
		UserID: 1, IdempotencyKey: "key-12345", PayloadHash: hash,
		State: "REJECTED", Reason: "LIMIT_EXCEEDED",
	}}
	gate := newFakeGate(10, item)
	svc := New(store, gate, testConfig())
	res, err := svc.Seckill(context.Background(), 1, "key-12345", item.ID, 1)
	if err != nil || res.HTTPStatus != 200 || res.Status != "REJECTED" || res.Reason != "LIMIT_EXCEEDED" {
		t.Fatalf("replay rejected: %+v %v", res, err)
	}
	if gate.reserveCalls != 0 {
		t.Fatalf("reserve calls=%d", gate.reserveCalls)
	}

	store.request.PayloadHash = "different"
	_, err = svc.Seckill(context.Background(), 1, "key-12345", item.ID, 1)
	assertAppCode(t, err, "IDEMPOTENCY_CONFLICT")
}

func TestRedisProcessingState(t *testing.T) {
	item := testItem()
	hash := PayloadHash(item.ID, 1)
	gate := newFakeGate(10, item)
	gate.states[stateKey(2, "key-12345")] = redisrepo.RequestState{
		Status: "PROCESSING", PayloadHash: hash, ReservationID: "res-1",
	}
	svc := New(&fakeStore{item: &item}, gate, testConfig())
	res, err := svc.Seckill(context.Background(), 2, "key-12345", item.ID, 1)
	if err != nil || res.HTTPStatus != 202 {
		t.Fatalf("processing: %+v %v", res, err)
	}
}

func TestUserRateLimitOnlyAppliesToNewIntent(t *testing.T) {
	item := testItem()
	hash := PayloadHash(item.ID, 1)
	order := &model.Order{OrderNo: "SK1", UserID: 1, ItemID: item.ID, Quantity: 1, PayloadHash: hash, Status: "PENDING_PAYMENT"}
	gate := newFakeGate(10, item)
	gate.denyUser = true
	svc := New(&fakeStore{item: &item, order: order}, gate, testConfig())
	res, err := svc.Seckill(context.Background(), 1, "key-12345", item.ID, 1)
	if err != nil || res.Order == nil {
		t.Fatalf("replay should bypass user limit: %+v %v", res, err)
	}

	svc = New(&fakeStore{item: &item}, gate, testConfig())
	_, err = svc.Seckill(context.Background(), 1, "key-67890", item.ID, 1)
	assertAppCode(t, err, "RATE_LIMITED")
}

func TestReserveBusinessErrors(t *testing.T) {
	item := testItem()
	cases := []struct {
		code string
		want string
		http int
	}{
		{"SOLD_OUT", "SOLD_OUT", 409},
		{"QUEUE_FULL", "QUEUE_FULL", 429},
		{"NOT_READY", "NOT_READY", 503},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			gate := newFakeGate(10, item)
			gate.reserveCode = tc.code
			svc := New(&fakeStore{item: &item}, gate, testConfig())
			_, err := svc.Seckill(context.Background(), 1, "key-12345", item.ID, 1)
			appErr := assertAppCode(t, err, tc.want)
			if appErr.HTTPStatus != tc.http {
				t.Fatalf("http=%d, want %d", appErr.HTTPStatus, tc.http)
			}
		})
	}
}

func TestReserveErrorIsUnavailable(t *testing.T) {
	item := testItem()
	gate := newFakeGate(10, item)
	gate.reserveErr = errors.New("redis write failed")
	svc := New(&fakeStore{item: &item}, gate, testConfig())
	_, err := svc.Seckill(context.Background(), 1, "key-12345", item.ID, 1)
	assertAppCode(t, err, "SERVICE_UNAVAILABLE")
}

func TestInputAndActivityBoundaries(t *testing.T) {
	svc := New(&fakeStore{}, newFakeGate(10, testItem()), testConfig())
	if _, err := svc.Seckill(context.Background(), 0, "key-12345", 1, 1); err.(*AppError).Code != "UNAUTHENTICATED" {
		t.Fatalf("missing user: %v", err)
	}
	if _, err := svc.Seckill(context.Background(), 1, "short", 1, 1); err.(*AppError).Code != "INVALID_ARGUMENT" {
		t.Fatalf("bad key: %v", err)
	}
	if _, err := svc.Seckill(context.Background(), 1, "key-12345", 0, 1); err.(*AppError).Code != "INVALID_ARGUMENT" {
		t.Fatalf("bad item: %v", err)
	}
	if _, err := svc.Seckill(context.Background(), 1, "key-12345", 1, 0); err.(*AppError).Code != "INVALID_ARGUMENT" {
		t.Fatalf("bad quantity: %v", err)
	}

	item := testItem()
	gate := newFakeGate(10, item)
	svc = New(&fakeStore{item: &item}, gate, testConfig())
	_, err := svc.Seckill(context.Background(), 1, "key-12345", item.ID, item.PerUserLimit+1)
	assertAppCode(t, err, "LIMIT_EXCEEDED")
	if gate.reserveCalls != 0 {
		t.Fatalf("reserve called for over-limit request")
	}
}

func TestItemNotFoundAndDBFallback(t *testing.T) {
	item := testItem()
	gate := newFakeGate(10, item)
	gate.item = nil
	svc := New(&fakeStore{itemErr: mysqlrepo.ErrItemNotFound}, gate, testConfig())
	_, err := svc.Seckill(context.Background(), 1, "key-12345", item.ID, 1)
	assertAppCode(t, err, "ITEM_NOT_FOUND")

	gate = newFakeGate(10, item)
	store := &fakeStore{item: &item, lookupErr: errors.New("mysql down")}
	svc = New(store, gate, testConfig())
	_, err = svc.Seckill(context.Background(), 1, "key-12345", item.ID, 1)
	assertAppCode(t, err, "SERVICE_UNAVAILABLE")
}

func TestRedisDownWithoutHistory(t *testing.T) {
	item := testItem()
	gate := newFakeGate(10, item)
	gate.allowErr = errors.New("redis down")
	svc := New(&fakeStore{item: &item}, gate, testConfig())
	_, err := svc.Seckill(context.Background(), 1, "key-12345", item.ID, 1)
	assertAppCode(t, err, "SERVICE_UNAVAILABLE")
	if gate.reserveCalls != 0 {
		t.Fatal("reserve must not be called")
	}
}

func TestRequestStatusSemantics(t *testing.T) {
	item := testItem()
	order := &model.Order{OrderNo: "SK1", UserID: 1, ItemID: item.ID, Status: "PENDING_PAYMENT"}
	store := &fakeStore{item: &item, order: order}
	svc := New(store, newFakeGate(10, item), testConfig())
	res, err := svc.GetRequestStatus(context.Background(), 1, "key-12345")
	if err != nil || res.HTTPStatus != 200 || res.Order == nil {
		t.Fatalf("order status: %+v %v", res, err)
	}

	store.order = nil
	store.request = &model.RequestResult{State: "REJECTED", Reason: "LIMIT_EXCEEDED"}
	res, err = svc.GetRequestStatus(context.Background(), 1, "key-12345")
	if err != nil || res.Status != "REJECTED" {
		t.Fatalf("rejected status: %+v %v", res, err)
	}

	store.request = &model.RequestResult{State: "PROCESSING"}
	res, err = svc.GetRequestStatus(context.Background(), 1, "key-12345")
	if err != nil || res.HTTPStatus != 202 {
		t.Fatalf("processing status: %+v %v", res, err)
	}

	store.request = nil
	gate := newFakeGate(10, item)
	gate.states[stateKey(1, "key-12345")] = redisrepo.RequestState{Status: "PROCESSING"}
	svc = New(store, gate, testConfig())
	res, err = svc.GetRequestStatus(context.Background(), 1, "key-12345")
	if err != nil || res.HTTPStatus != 202 {
		t.Fatalf("redis processing: %+v %v", res, err)
	}

	gate.states = map[string]redisrepo.RequestState{}
	_, err = svc.GetRequestStatus(context.Background(), 1, "key-12345")
	assertAppCode(t, err, "RESULT_UNKNOWN")

	store.lookupErr = errors.New("mysql down")
	_, err = svc.GetRequestStatus(context.Background(), 1, "key-12345")
	assertAppCode(t, err, "SERVICE_UNAVAILABLE")
}

func TestOrderAndPaymentErrorMapping(t *testing.T) {
	item := testItem()
	store := &fakeStore{item: &item}
	svc := New(store, newFakeGate(10, item), testConfig())
	store.orderErr = mysqlrepo.ErrNotFound
	_, err := svc.GetOrder(context.Background(), 1, "SK404")
	assertAppCode(t, err, "ORDER_NOT_FOUND")

	store.orderErr = nil
	store.payErr = mysqlrepo.ErrPaymentAmount
	_, err = svc.PayMock(context.Background(), 1, "SK1", "txn", 1)
	assertAppCode(t, err, "INVALID_ARGUMENT")

	store.payErr = mysqlrepo.ErrPayAfterCancel
	_, err = svc.PayMock(context.Background(), 1, "SK1", "txn", 1)
	assertAppCode(t, err, "PAY_AFTER_CANCEL")

	store.payErr = mysqlrepo.ErrNotFound
	_, err = svc.PayMock(context.Background(), 1, "SK1", "txn", 1)
	assertAppCode(t, err, "ORDER_NOT_FOUND")

	paidAt := time.Now()
	store.payErr = nil
	store.payResult = &model.Order{OrderNo: "SK1", Status: "PAID", PaidAt: &paidAt}
	order, err := svc.PayMock(context.Background(), 1, "SK1", "txn", 1)
	if err != nil || order.Status != "PAID" {
		t.Fatalf("pay success: %+v %v", order, err)
	}
}

func TestActivityCreationWarningsAndStockView(t *testing.T) {
	item := testItem()
	activity := &model.Activity{ID: 1, Name: "a", Status: "ACTIVE"}
	store := &fakeStore{activity: activity, items: []model.Item{item}}
	gate := newFakeGate(10, item)
	gate.setStockErr = errors.New("redis stock failed")
	gate.snapshotErr = errors.New("redis snapshot failed")
	svc := New(store, gate, testConfig())
	result, err := svc.CreateActivity(context.Background(), model.ActivityInput{
		Name: "a", StartAt: time.Now().Add(-time.Hour), EndAt: time.Now().Add(time.Hour),
		Items: []model.ItemInput{{SKUCode: "SKU", Name: "item", PriceCents: 1, TotalStock: 10, PerUserLimit: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 1 || result.Items[0].RedisReady || len(result.Warnings) == 0 {
		t.Fatalf("unexpected create result: %+v", result)
	}

	gate = newFakeGate(7, item)
	svc = New(store, gate, testConfig())
	stock, err := svc.GetStock(context.Background(), activity.ID, true)
	if err != nil || len(stock.Items) != 1 || stock.Items[0].RedisStock == nil || *stock.Items[0].RedisStock != 7 {
		t.Fatalf("stock view: %+v %v", stock, err)
	}
}

func TestCancelExpiredRestocksAndStoreError(t *testing.T) {
	item := testItem()
	store := &fakeStore{cancelled: []model.CancelledOrder{{OrderNo: "SK1", ItemID: item.ID, UserID: 1, Quantity: 1}}}
	gate := newFakeGate(10, item)
	svc := New(store, gate, testConfig())
	n, err := svc.CancelExpired(context.Background())
	if err != nil || n != 1 || gate.restockCalls != 1 {
		t.Fatalf("cancel: n=%d calls=%d err=%v", n, gate.restockCalls, err)
	}

	store.cancelErr = errors.New("mysql down")
	_, err = svc.CancelExpired(context.Background())
	if err == nil {
		t.Fatal("expected cancel error")
	}

	store.cancelErr = nil
	gate.restockErr = errors.New("redis down")
	n, err = svc.CancelExpired(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("redis restock failure must not fail DB cancel: n=%d err=%v", n, err)
	}
}

func TestRequestIDDeterministic(t *testing.T) {
	a := RequestID(1, "key-12345")
	b := RequestID(1, "key-12345")
	c := RequestID(2, "key-12345")
	if a != b || a == c {
		t.Fatalf("request ids: %s %s %s", a, b, c)
	}
}

func assertAppCode(t *testing.T, err error, code string) *AppError {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error %s", code)
	}
	appErr, ok := AsAppError(err)
	if !ok || appErr.Code != code {
		t.Fatalf("error=%v, want code %s", err, code)
	}
	return appErr
}
