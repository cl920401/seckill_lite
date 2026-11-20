package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"seckill_lite/internal/config"
	"seckill_lite/internal/model"
	"seckill_lite/internal/service"
)

type Service interface {
	Seckill(ctx context.Context, userID int64, idempotencyKey string, itemID int64, quantity int) (*service.SeckillResult, error)
	GetRequestStatus(ctx context.Context, userID int64, key string) (*service.RequestStatusResult, error)
	GetOrder(ctx context.Context, userID int64, orderNo string) (*model.Order, error)
	ListOrders(ctx context.Context, userID int64, limit int, beforeID int64) ([]model.Order, error)
	CreateActivity(ctx context.Context, in model.ActivityInput) (*service.CreateActivityResult, error)
	GetStock(ctx context.Context, activityID int64, includeRedis bool) (*service.StockResult, error)
	PayMock(ctx context.Context, userID int64, orderNo, providerTxnID string, amount int64) (*model.Order, error)
}

type API struct {
	svc       Service
	cfg       config.Config
	readiness func(context.Context) error
	log       *slog.Logger
}

type contextKey string

const requestIDKey contextKey = "requestId"

func New(svc Service, cfg config.Config, readiness func(context.Context) error) http.Handler {
	api := &API{svc: svc, cfg: cfg, readiness: readiness, log: slog.Default()}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/admin/activities", api.createActivity)
	mux.HandleFunc("GET /api/v1/admin/activities/{id}/stock", api.getStock)
	mux.HandleFunc("POST /api/v1/seckill/orders", api.seckill)
	mux.HandleFunc("GET /api/v1/seckill/requests/{key}", api.requestStatus)
	mux.HandleFunc("GET /api/v1/orders/{orderNo}", api.getOrder)
	mux.HandleFunc("GET /api/v1/orders", api.listOrders)
	mux.HandleFunc("POST /api/v1/orders/{orderNo}/pay/mock", api.payMock)
	mux.HandleFunc("GET /healthz", api.health)
	mux.HandleFunc("GET /readyz", api.ready)
	return api.requestID(api.logging(api.recoverer(api.bodyLimit(api.timeout(10*time.Second, mux)))))
}

// seckill 将 HTTP 参数交给服务层执行准入，并把受理/终态映射到响应；本层不直接扣库存或建单。
// X-User-Id 是 MVP 的演示身份入口，业务幂等键按该用户隔离。
func (a *API) seckill(w http.ResponseWriter, r *http.Request) {
	userID, err := a.userID(r)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	var body struct {
		ItemID   int64 `json:"itemId"`
		Quantity int   `json:"quantity"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		a.writeError(w, r, service.ErrInvalidArgument)
		return
	}
	if body.Quantity == 0 {
		body.Quantity = 1
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	result, err := a.svc.Seckill(r.Context(), userID, key, body.ItemID, body.Quantity)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	if result.HTTPStatus == http.StatusAccepted {
		// 202 只表示预约已受理或仍在处理，不能当作订单创建成功；客户端按原幂等键轮询。
		writeJSON(w, http.StatusAccepted, map[string]interface{}{
			"requestId":      result.RequestID,
			"idempotencyKey": result.IdempotencyKey,
			"status":         "PROCESSING",
			"pollUrl":        "/api/v1/seckill/requests/" + result.IdempotencyKey,
			"retryAfterMs":   result.RetryAfterMs,
		})
		return
	}
	if result.Status == "REJECTED" {
		// 200 表示成功读到了稳定的拒绝结果，购买是否成功还要看 status 字段。
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"requestId":      result.RequestID,
			"idempotencyKey": result.IdempotencyKey,
			"status":         "REJECTED",
			"reason":         result.Reason,
		})
		return
	}
	if result.Order == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"requestId":      result.RequestID,
			"idempotencyKey": result.IdempotencyKey,
			"status":         "SUCCEEDED",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"requestId":      result.RequestID,
		"idempotencyKey": result.IdempotencyKey,
		"status":         "SUCCEEDED",
		"orderNo":        result.Order.OrderNo,
		"orderStatus":    result.Order.Status,
		"itemId":         result.Order.ItemID,
		"quantity":       result.Order.Quantity,
		"expireAt":       result.Order.ExpireAt,
	})
}

// requestStatus 查询购买请求的处理结果；SUCCEEDED 表示已建单，订单本身仍可能待支付或已取消。
func (a *API) requestStatus(w http.ResponseWriter, r *http.Request) {
	userID, err := a.userID(r)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	key := r.PathValue("key")
	result, err := a.svc.GetRequestStatus(r.Context(), userID, key)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	if result.HTTPStatus == http.StatusAccepted {
		writeJSON(w, http.StatusAccepted, map[string]interface{}{
			"requestId":    service.RequestID(userID, key),
			"status":       "PROCESSING",
			"retryAfterMs": result.RetryAfterMs,
		})
		return
	}
	if result.Status == "REJECTED" {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"status": "REJECTED",
			"reason": result.Reason,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status": "SUCCEEDED",
		"order":  result.Order,
	})
}

func (a *API) getOrder(w http.ResponseWriter, r *http.Request) {
	userID, err := a.userID(r)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	order, err := a.svc.GetOrder(r.Context(), userID, r.PathValue("orderNo"))
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, order)
}

func (a *API) listOrders(w http.ResponseWriter, r *http.Request) {
	userID, err := a.userID(r)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	if q := r.URL.Query().Get("userId"); q != "" {
		parsed, parseErr := strconv.ParseInt(q, 10, 64)
		if parseErr != nil || parsed != userID {
			a.writeError(w, r, service.ErrInvalidArgument)
			return
		}
	}
	limit := 20
	if q := r.URL.Query().Get("limit"); q != "" {
		if n, parseErr := strconv.Atoi(q); parseErr == nil && n > 0 && n <= 100 {
			limit = n
		} else {
			a.writeError(w, r, service.ErrInvalidArgument)
			return
		}
	}
	var beforeID int64
	if q := r.URL.Query().Get("beforeId"); q != "" {
		n, parseErr := strconv.ParseInt(q, 10, 64)
		if parseErr != nil || n < 0 {
			a.writeError(w, r, service.ErrInvalidArgument)
			return
		}
		beforeID = n
	}
	orders, err := a.svc.ListOrders(r.Context(), userID, limit, beforeID)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	var next int64
	if len(orders) > 0 {
		next = orders[len(orders)-1].ID
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"items":        orders,
		"nextBeforeId": next,
	})
}

// payMock 仅模拟支付状态变更；订单归属、金额与支付/取消竞争在服务层和 DB 事务中校验。
func (a *API) payMock(w http.ResponseWriter, r *http.Request) {
	userID, err := a.userID(r)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	var body struct {
		ProviderTxnID string `json:"providerTxnId"`
		AmountCents   int64  `json:"amountCents"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		a.writeError(w, r, service.ErrInvalidArgument)
		return
	}
	order, err := a.svc.PayMock(r.Context(), userID, r.PathValue("orderNo"), body.ProviderTxnID, body.AmountCents)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"orderNo":       order.OrderNo,
		"status":        order.Status,
		"paidAt":        order.PaidAt,
		"providerTxnId": body.ProviderTxnID,
	})
}

// createActivity 创建活动和商品；201 表示 DB 创建成功，Redis 库存是否可用另看 redisReady/warnings。
func (a *API) createActivity(w http.ResponseWriter, r *http.Request) {
	if err := a.requireAdmin(r); err != nil {
		a.writeError(w, r, err)
		return
	}
	var body struct {
		Name    string    `json:"name"`
		StartAt time.Time `json:"startAt"`
		EndAt   time.Time `json:"endAt"`
		Items   []struct {
			SKUCode      string `json:"skuCode"`
			Name         string `json:"name"`
			PriceCents   int64  `json:"priceCents"`
			TotalStock   int    `json:"totalStock"`
			PerUserLimit int    `json:"perUserLimit"`
		} `json:"items"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		a.writeError(w, r, service.ErrInvalidArgument)
		return
	}
	input := model.ActivityInput{Name: body.Name, StartAt: body.StartAt, EndAt: body.EndAt}
	for _, item := range body.Items {
		input.Items = append(input.Items, model.ItemInput{
			SKUCode: item.SKUCode, Name: item.Name, PriceCents: item.PriceCents,
			TotalStock: item.TotalStock, PerUserLimit: item.PerUserLimit,
		})
	}
	result, err := a.svc.CreateActivity(r.Context(), input)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	items := make([]map[string]interface{}, 0, len(result.Items))
	for _, item := range result.Items {
		items = append(items, map[string]interface{}{
			"itemId":         item.ID,
			"skuCode":        item.SKUCode,
			"availableStock": item.AvailableStock,
			"redisReady":     item.RedisReady,
		})
	}
	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"activityId": result.Activity.ID,
		"items":      items,
		"warnings":   result.Warnings,
	})
}

func (a *API) getStock(w http.ResponseWriter, r *http.Request) {
	if err := a.requireAdmin(r); err != nil {
		a.writeError(w, r, err)
		return
	}
	activityID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || activityID <= 0 {
		a.writeError(w, r, service.ErrInvalidArgument)
		return
	}
	includeRedis := r.URL.Query().Get("includeRedis") == "true"
	result, err := a.svc.GetStock(r.Context(), activityID, includeRedis)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	items := make([]map[string]interface{}, 0, len(result.Items))
	for _, item := range result.Items {
		entry := map[string]interface{}{
			"itemId":         item.ID,
			"skuCode":        item.SKUCode,
			"totalStock":     item.TotalStock,
			"availableStock": item.AvailableStock,
			"version":        item.Version,
		}
		if includeRedis {
			redisView := map[string]interface{}{"ready": item.RedisReady}
			if item.RedisStock != nil {
				redisView["stock"] = *item.RedisStock
			}
			if item.RedisError != "" {
				redisView["error"] = item.RedisError
			}
			entry["redis"] = redisView
		}
		items = append(items, entry)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"activityId": result.Activity.ID,
		"items":      items,
		"warnings":   result.Warnings,
	})
}

func (a *API) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *API) ready(w http.ResponseWriter, r *http.Request) {
	if a.readiness == nil {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 500*time.Millisecond)
	defer cancel()
	if err := a.readiness(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{
			"status": "not_ready",
			"error":  err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (a *API) userID(r *http.Request) (int64, error) {
	raw := strings.TrimSpace(r.Header.Get("X-User-Id"))
	if raw == "" {
		return 0, service.ErrUnauthenticated
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, service.ErrUnauthenticated
	}
	return id, nil
}

func (a *API) requireAdmin(r *http.Request) error {
	got := r.Header.Get("X-Admin-Token")
	if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(a.cfg.AdminToken)) != 1 {
		return service.ErrUnauthenticated
	}
	return nil
}

// writeError 区分明确业务错误和未预期异常；503/429 不能作为换幂等键或自动回补的依据。
func (a *API) writeError(w http.ResponseWriter, r *http.Request, err error) {
	appErr, ok := service.AsAppError(err)
	if !ok {
		a.log.ErrorContext(r.Context(), "unexpected handler error", "error", err)
		appErr = &service.AppError{Code: "INTERNAL", Message: "internal error", HTTPStatus: http.StatusInternalServerError}
	}
	if appErr.HTTPStatus == http.StatusTooManyRequests || appErr.HTTPStatus == http.StatusServiceUnavailable {
		w.Header().Set("Retry-After", "1")
	}
	writeJSON(w, appErr.HTTPStatus, map[string]interface{}{
		"code":      appErr.Code,
		"message":   appErr.Message,
		"requestId": requestIDFrom(r.Context()),
	})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst interface{}) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("multiple JSON values")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, payload interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func (a *API) requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(r.Header.Get("X-Request-Id"))
		if id == "" {
			id = newRequestID()
		}
		w.Header().Set("X-Request-Id", id)
		ctx := context.WithValue(r.Context(), requestIDKey, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (a *API) bodyLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		next.ServeHTTP(w, r)
	})
}

func (a *API) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				a.log.ErrorContext(r.Context(), "panic recovered", "panic", rec, "stack", string(debug.Stack()))
				writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
					"code":      "INTERNAL",
					"message":   "internal error",
					"requestId": requestIDFrom(r.Context()),
				})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (a *API) timeout(d time.Duration, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), d)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (a *API) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		a.log.InfoContext(r.Context(), "http request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"durationMs", time.Since(start).Milliseconds(),
			"requestId", requestIDFrom(r.Context()),
		)
	})
}

func requestIDFrom(ctx context.Context) string {
	v, _ := ctx.Value(requestIDKey).(string)
	return v
}

func newRequestID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "req_fallback"
	}
	return "req_" + hex.EncodeToString(b[:])
}
