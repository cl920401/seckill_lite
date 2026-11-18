package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"seckill_lite/internal/config"
	"seckill_lite/internal/model"
	"seckill_lite/internal/service"
)

type fakeService struct {
	seckillResult *service.SeckillResult
	seckillErr    error
	statusResult  *service.RequestStatusResult
	statusErr     error
	order         *model.Order
	orderErr      error
	orders        []model.Order
	ordersErr     error
	createResult  *service.CreateActivityResult
	createErr     error
	stockResult   *service.StockResult
	stockErr      error
	payOrder      *model.Order
	payErr        error
	panicSeckill  bool
	lastQuantity  int
}

func (f *fakeService) Seckill(ctx context.Context, userID int64, key string, itemID int64, quantity int) (*service.SeckillResult, error) {
	if f.panicSeckill {
		panic("test panic")
	}
	f.lastQuantity = quantity
	return f.seckillResult, f.seckillErr
}
func (f *fakeService) GetRequestStatus(ctx context.Context, userID int64, key string) (*service.RequestStatusResult, error) {
	return f.statusResult, f.statusErr
}
func (f *fakeService) GetOrder(ctx context.Context, userID int64, orderNo string) (*model.Order, error) {
	return f.order, f.orderErr
}
func (f *fakeService) ListOrders(ctx context.Context, userID int64, limit int, beforeID int64) ([]model.Order, error) {
	return f.orders, f.ordersErr
}
func (f *fakeService) CreateActivity(ctx context.Context, in model.ActivityInput) (*service.CreateActivityResult, error) {
	return f.createResult, f.createErr
}
func (f *fakeService) GetStock(ctx context.Context, activityID int64, includeRedis bool) (*service.StockResult, error) {
	return f.stockResult, f.stockErr
}
func (f *fakeService) PayMock(ctx context.Context, userID int64, orderNo, txn string, amount int64) (*model.Order, error) {
	return f.payOrder, f.payErr
}

func testHandler(svc Service) http.Handler {
	return New(svc, config.Config{AdminToken: "admin-token"}, func(ctx context.Context) error { return nil })
}

func doRequest(t *testing.T, h http.Handler, method, path string, body interface{}, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestSeckillAcceptedAndQuantityDefault(t *testing.T) {
	svc := &fakeService{seckillResult: &service.SeckillResult{
		HTTPStatus: 202, RequestID: "req_1", IdempotencyKey: "key-12345", RetryAfterMs: 500,
	}}
	h := testHandler(svc)
	rec := doRequest(t, h, http.MethodPost, "/api/v1/seckill/orders",
		map[string]interface{}{"itemId": 1}, map[string]string{"X-User-Id": "1", "Idempotency-Key": "key-12345"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	if svc.lastQuantity != 1 {
		t.Fatalf("quantity=%d, want 1", svc.lastQuantity)
	}
	if rec.Header().Get("X-Request-Id") == "" {
		t.Fatal("missing request id")
	}
}

func TestSeckillRequiresUser(t *testing.T) {
	h := testHandler(&fakeService{})
	rec := doRequest(t, h, http.MethodPost, "/api/v1/seckill/orders",
		map[string]interface{}{"itemId": 1}, map[string]string{"Idempotency-Key": "key-12345"})
	assertCode(t, rec, http.StatusUnauthorized, "UNAUTHENTICATED")
}

func TestSeckillRejectsUnknownField(t *testing.T) {
	h := testHandler(&fakeService{})
	rec := doRequest(t, h, http.MethodPost, "/api/v1/seckill/orders",
		map[string]interface{}{"itemId": 1, "unknown": true},
		map[string]string{"X-User-Id": "1", "Idempotency-Key": "key-12345"})
	assertCode(t, rec, http.StatusBadRequest, "INVALID_ARGUMENT")
}

func TestSeckillErrorMappingAndRetryAfter(t *testing.T) {
	svc := &fakeService{seckillErr: service.ErrRateLimited}
	h := testHandler(svc)
	rec := doRequest(t, h, http.MethodPost, "/api/v1/seckill/orders",
		map[string]interface{}{"itemId": 1}, map[string]string{"X-User-Id": "1", "Idempotency-Key": "key-12345"})
	assertCode(t, rec, http.StatusTooManyRequests, "RATE_LIMITED")
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("missing Retry-After")
	}
}

func TestSeckillReplayRejected(t *testing.T) {
	svc := &fakeService{seckillResult: &service.SeckillResult{
		HTTPStatus: 200, Status: "REJECTED", Reason: "LIMIT_EXCEEDED", RequestID: "req_1", IdempotencyKey: "key-12345",
	}}
	h := testHandler(svc)
	rec := doRequest(t, h, http.MethodPost, "/api/v1/seckill/orders",
		map[string]interface{}{"itemId": 1, "quantity": 1}, map[string]string{"X-User-Id": "1", "Idempotency-Key": "key-12345"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	var payload map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &payload)
	if payload["status"] != "REJECTED" || payload["reason"] != "LIMIT_EXCEEDED" {
		t.Fatalf("payload=%v", payload)
	}
}

func TestRequestStatusAndUnknown(t *testing.T) {
	svc := &fakeService{statusResult: &service.RequestStatusResult{HTTPStatus: 202, Status: "PROCESSING", RetryAfterMs: 500}}
	h := testHandler(svc)
	rec := doRequest(t, h, http.MethodGet, "/api/v1/seckill/requests/key-12345", nil, map[string]string{"X-User-Id": "1"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status=%d", rec.Code)
	}

	svc.statusResult = nil
	svc.statusErr = service.ErrResultUnknown
	rec = doRequest(t, h, http.MethodGet, "/api/v1/seckill/requests/key-12345", nil, map[string]string{"X-User-Id": "1"})
	assertCode(t, rec, http.StatusConflict, "RESULT_UNKNOWN")
}

func TestOrderListRejectsUserIdMismatch(t *testing.T) {
	svc := &fakeService{orders: []model.Order{{ID: 10, OrderNo: "SK1", Status: "PENDING_PAYMENT", CreatedAt: time.Now()}}}
	h := testHandler(svc)
	rec := doRequest(t, h, http.MethodGet, "/api/v1/orders?userId=2", nil, map[string]string{"X-User-Id": "1"})
	assertCode(t, rec, http.StatusBadRequest, "INVALID_ARGUMENT")
	rec = doRequest(t, h, http.MethodGet, "/api/v1/orders?beforeId=10", nil, map[string]string{"X-User-Id": "1"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
}

func TestOrderNotFound(t *testing.T) {
	h := testHandler(&fakeService{orderErr: service.ErrOrderNotFound})
	rec := doRequest(t, h, http.MethodGet, "/api/v1/orders/SK404", nil, map[string]string{"X-User-Id": "1"})
	assertCode(t, rec, http.StatusNotFound, "ORDER_NOT_FOUND")
}

func TestPayMock(t *testing.T) {
	paid := time.Now()
	svc := &fakeService{payOrder: &model.Order{OrderNo: "SK1", Status: "PAID", PaidAt: &paid}}
	h := testHandler(svc)
	rec := doRequest(t, h, http.MethodPost, "/api/v1/orders/SK1/pay/mock",
		map[string]interface{}{"providerTxnId": "txn-1", "amountCents": 100},
		map[string]string{"X-User-Id": "1"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	svc.payOrder = nil
	svc.payErr = service.ErrPayAfterCancel
	rec = doRequest(t, h, http.MethodPost, "/api/v1/orders/SK1/pay/mock",
		map[string]interface{}{"providerTxnId": "txn-1", "amountCents": 100},
		map[string]string{"X-User-Id": "1"})
	assertCode(t, rec, http.StatusConflict, "PAY_AFTER_CANCEL")
}

func TestAdminAuthAndCreate(t *testing.T) {
	svc := &fakeService{createResult: &service.CreateActivityResult{
		Activity: &model.Activity{ID: 9},
		Items: []service.ItemReadiness{{
			Item: model.Item{ID: 10, SKUCode: "SKU", AvailableStock: 5, TotalStock: 5}, RedisReady: true,
		}},
	}}
	h := testHandler(svc)
	body := map[string]interface{}{
		"name": "demo", "startAt": time.Now().Add(-time.Hour), "endAt": time.Now().Add(time.Hour),
		"items": []map[string]interface{}{{"skuCode": "SKU", "name": "item", "priceCents": 100, "totalStock": 5, "perUserLimit": 1}},
	}
	rec := doRequest(t, h, http.MethodPost, "/api/v1/admin/activities", body, map[string]string{"X-Admin-Token": "wrong"})
	assertCode(t, rec, http.StatusUnauthorized, "UNAUTHENTICATED")
	rec = doRequest(t, h, http.MethodPost, "/api/v1/admin/activities", body, map[string]string{"X-Admin-Token": "admin-token"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
}

func TestReadyFailure(t *testing.T) {
	h := New(&fakeService{}, config.Config{AdminToken: "admin-token"}, func(ctx context.Context) error {
		return errors.New("redis down")
	})
	rec := doRequest(t, h, http.MethodGet, "/readyz", nil, nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
}

func TestPanicRecovered(t *testing.T) {
	h := testHandler(&fakeService{panicSeckill: true})
	rec := doRequest(t, h, http.MethodPost, "/api/v1/seckill/orders",
		map[string]interface{}{"itemId": 1}, map[string]string{"X-User-Id": "1", "Idempotency-Key": "key-12345"})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
}

func assertCode(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status=%d, want %d, body=%s", rec.Code, status, rec.Body)
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode body: %v body=%s", err, rec.Body)
	}
	if payload["code"] != code {
		t.Fatalf("code=%v, want %s, body=%s", payload["code"], code, rec.Body)
	}
}
