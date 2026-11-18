package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"seckill_lite/internal/config"
	"seckill_lite/internal/model"
	"seckill_lite/internal/mysqlrepo"
	"seckill_lite/internal/redisrepo"
)

var idempotencyKeyPattern = regexp.MustCompile(`^[A-Za-z0-9._~-]{8,128}$`)

type Store interface {
	CreateActivity(ctx context.Context, in model.ActivityInput) (*model.Activity, []model.Item, error)
	GetItem(ctx context.Context, itemID int64) (*model.Item, error)
	GetActivityStock(ctx context.Context, activityID int64) (*model.Activity, []model.Item, error)
	LookupRequest(ctx context.Context, userID int64, key string) (*model.Order, *model.RequestResult, error)
	GetOrderByNo(ctx context.Context, orderNo string, userID int64) (*model.Order, error)
	ListOrders(ctx context.Context, userID int64, limit int, beforeID int64) ([]model.Order, error)
	MarkPaid(ctx context.Context, userID int64, orderNo, providerTxnID string, amount int64) (*model.Order, error)
	CancelExpired(ctx context.Context, limit int) ([]model.CancelledOrder, error)
}

type Gate interface {
	Allow(ctx context.Context, key string, rate float64, burst, cost int) (bool, int, error)
	GetRequest(ctx context.Context, userID int64, key string) (*redisrepo.RequestState, error)
	Reserve(ctx context.Context, req redisrepo.ReserveRequest) (redisrepo.ReserveResult, error)
	MarkSuccess(ctx context.Context, userID int64, key, reservationID, orderNo string) error
	Compensate(ctx context.Context, itemID int64, userID int64, key, reservationID string, quantity int, reason string) (string, error)
	Restock(ctx context.Context, itemID int64, orderNo string, quantity int) (string, error)
	GetStock(ctx context.Context, itemID int64) (int64, bool, error)
	SetStockNX(ctx context.Context, itemID int64, stock int) error
	SetItemSnapshot(ctx context.Context, item model.Item, ttl time.Duration) error
	GetItemSnapshot(ctx context.Context, itemID int64) (*model.Item, bool, error)
	IngressKey() string
	QueryKey() string
	UserKey(userID int64) string
}

type Service struct {
	store    Store
	gate     Gate
	cfg      config.Config
	querySem chan struct{}
}

type SeckillResult struct {
	HTTPStatus     int
	Status         string
	RequestID      string
	IdempotencyKey string
	Order          *model.Order
	Reason         string
	RetryAfterMs   int
}

type RequestStatusResult struct {
	HTTPStatus   int
	Status       string
	Reason       string
	Order        *model.Order
	RetryAfterMs int
}

type ItemReadiness struct {
	model.Item
	RedisReady bool
}

type CreateActivityResult struct {
	Activity *model.Activity
	Items    []ItemReadiness
	Warnings []string
}

type StockItem struct {
	model.Item
	RedisReady bool
	RedisStock *int64
	RedisError string
}

type StockResult struct {
	Activity *model.Activity
	Items    []StockItem
	Warnings []string
}

func New(store Store, gate Gate, cfg config.Config) *Service {
	limit := cfg.HistoryLookups
	if limit <= 0 {
		limit = 32
	}
	return &Service{store: store, gate: gate, cfg: cfg, querySem: make(chan struct{}, limit)}
}

func (s *Service) Seckill(ctx context.Context, userID int64, idempotencyKey string, itemID int64, quantity int) (*SeckillResult, error) {
	if userID <= 0 {
		return nil, ErrUnauthenticated
	}
	if !idempotencyKeyPattern.MatchString(idempotencyKey) {
		return nil, ErrIdempotencyKeyNeeded
	}
	if itemID <= 0 || quantity <= 0 || quantity > 1_000_000 {
		return nil, ErrInvalidArgument
	}
	payloadHash := PayloadHash(itemID, quantity)
	requestID := RequestID(userID, idempotencyKey)

	allowed, _, limitErr := s.gate.Allow(ctx, s.gate.IngressKey(), s.cfg.IngressRPS, s.cfg.IngressBurst, 1)
	if limitErr != nil {
		if res, found, err := s.lookupHistoryGuarded(ctx, userID, idempotencyKey, payloadHash, requestID); err != nil {
			return nil, err
		} else if found {
			return res, nil
		}
		return nil, wrap(ErrServiceUnavailable, limitErr)
	}
	if !allowed {
		return nil, wrap(ErrRateLimited, nil)
	}

	state, redisErr := s.gate.GetRequest(ctx, userID, idempotencyKey)
	if redisErr != nil {
		if res, found, err := s.lookupHistoryGuarded(ctx, userID, idempotencyKey, payloadHash, requestID); err != nil {
			return nil, err
		} else if found {
			return res, nil
		}
		return nil, wrap(ErrServiceUnavailable, redisErr)
	}
	if state != nil {
		// MySQL is authoritative. Consult durable history before trusting the
		// Redis fingerprint, which may be stale or polluted.
		res, found, err := s.lookupHistoryGuarded(ctx, userID, idempotencyKey, payloadHash, requestID)
		if err != nil {
			return nil, err
		}
		if found {
			return res, nil
		}
		if state.PayloadHash != "" && state.PayloadHash != payloadHash {
			return nil, wrap(ErrIdempotencyConflict, nil)
		}
		switch state.Status {
		case "PROCESSING":
			return processingResult(requestID, idempotencyKey), nil
		case "REJECTED":
			return &SeckillResult{HTTPStatus: 200, Status: "REJECTED", RequestID: requestID, IdempotencyKey: idempotencyKey, Reason: state.Reason}, nil
		default:
			return nil, wrap(ErrResultUnknown, nil)
		}
	}

	if res, found, err := s.lookupHistoryGuarded(ctx, userID, idempotencyKey, payloadHash, requestID); err != nil {
		return nil, err
	} else if found {
		return res, nil
	}

	allowed, _, err := s.gate.Allow(ctx, s.gate.UserKey(userID), s.cfg.UserRPS, s.cfg.UserBurst, 1)
	if err != nil {
		return nil, wrap(ErrServiceUnavailable, err)
	}
	if !allowed {
		return nil, wrap(ErrRateLimited, nil)
	}

	item, err := s.loadItem(ctx, itemID)
	if err != nil {
		return nil, err
	}
	if err := validateItemWindow(item, quantity); err != nil {
		return nil, err
	}

	reservationID, err := randomID()
	if err != nil {
		return nil, wrap(ErrServiceUnavailable, err)
	}
	reserveResult, err := s.gate.Reserve(ctx, redisrepo.ReserveRequest{
		UserID:        userID,
		Key:           idempotencyKey,
		PayloadHash:   payloadHash,
		ReservationID: reservationID,
		ItemID:        itemID,
		Quantity:      quantity,
		AcceptedAt:    time.Now().UTC(),
		TTL:           s.cfg.RequestTTL,
		MaxEntries:    s.cfg.StreamMax,
	})
	if err != nil {
		return nil, wrap(ErrServiceUnavailable, err)
	}
	switch reserveResult.Code {
	case "ACCEPTED":
		return processingResult(requestID, idempotencyKey), nil
	case "DUPLICATE":
		if res, found, err := s.lookupHistoryGuarded(ctx, userID, idempotencyKey, payloadHash, requestID); err != nil {
			return nil, err
		} else if found {
			return res, nil
		}
		switch reserveResult.Status {
		case "PROCESSING":
			return processingResult(requestID, idempotencyKey), nil
		case "REJECTED":
			return &SeckillResult{HTTPStatus: 200, Status: "REJECTED", RequestID: requestID, IdempotencyKey: idempotencyKey}, nil
		default:
			return nil, wrap(ErrResultUnknown, nil)
		}
	case "CONFLICT":
		// The Lua fingerprint may be stale; MySQL history wins when present.
		if res, found, err := s.lookupHistoryGuarded(ctx, userID, idempotencyKey, payloadHash, requestID); err != nil {
			return nil, err
		} else if found {
			return res, nil
		}
		return nil, wrap(ErrIdempotencyConflict, nil)
	case "SOLD_OUT":
		return nil, wrap(ErrSoldOut, nil)
	case "NOT_READY":
		return nil, wrap(ErrNotReady, nil)
	case "QUEUE_FULL":
		return nil, wrap(ErrQueueFull, nil)
	default:
		return nil, wrap(ErrServiceUnavailable, fmt.Errorf("unknown reserve result %q", reserveResult.Code))
	}
}

func (s *Service) GetRequestStatus(ctx context.Context, userID int64, key string) (*RequestStatusResult, error) {
	if userID <= 0 {
		return nil, ErrUnauthenticated
	}
	if !idempotencyKeyPattern.MatchString(key) {
		return nil, ErrIdempotencyKeyNeeded
	}
	release, err := s.allowQuery(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	order, req, err := s.store.LookupRequest(ctx, userID, key)
	if err != nil {
		return nil, wrap(ErrServiceUnavailable, err)
	}
	requestID := RequestID(userID, key)
	if order != nil {
		return &RequestStatusResult{HTTPStatus: 200, Status: "SUCCEEDED", Order: order}, nil
	}
	if req != nil {
		switch req.State {
		case "REJECTED":
			return &RequestStatusResult{HTTPStatus: 200, Status: "REJECTED", Reason: req.Reason}, nil
		case "PROCESSING":
			return &RequestStatusResult{HTTPStatus: 202, Status: "PROCESSING", RetryAfterMs: 500}, nil
		case "SUCCEEDED":
			return nil, wrap(ErrResultUnknown, fmt.Errorf("request %s is succeeded without order", requestID))
		default:
			return nil, wrap(ErrResultUnknown, nil)
		}
	}
	state, err := s.gate.GetRequest(ctx, userID, key)
	if err != nil {
		return nil, wrap(ErrServiceUnavailable, err)
	}
	if state == nil {
		return nil, wrap(ErrResultUnknown, nil)
	}
	switch state.Status {
	case "PROCESSING":
		return &RequestStatusResult{HTTPStatus: 202, Status: "PROCESSING", RetryAfterMs: 500}, nil
	case "REJECTED":
		return &RequestStatusResult{HTTPStatus: 200, Status: "REJECTED", Reason: state.Reason}, nil
	default:
		return nil, wrap(ErrResultUnknown, nil)
	}
}

func (s *Service) GetOrder(ctx context.Context, userID int64, orderNo string) (*model.Order, error) {
	if userID <= 0 {
		return nil, ErrUnauthenticated
	}
	if orderNo == "" {
		return nil, ErrOrderNotFound
	}
	release, err := s.allowQuery(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	order, err := s.store.GetOrderByNo(ctx, orderNo, userID)
	if errors.Is(err, mysqlrepo.ErrNotFound) {
		return nil, ErrOrderNotFound
	}
	if err != nil {
		return nil, wrap(ErrServiceUnavailable, err)
	}
	return order, nil
}

func (s *Service) ListOrders(ctx context.Context, userID int64, limit int, beforeID int64) ([]model.Order, error) {
	if userID <= 0 {
		return nil, ErrUnauthenticated
	}
	release, err := s.allowQuery(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	orders, err := s.store.ListOrders(ctx, userID, limit, beforeID)
	if err != nil {
		return nil, wrap(ErrServiceUnavailable, err)
	}
	return orders, nil
}

func (s *Service) CreateActivity(ctx context.Context, in model.ActivityInput) (*CreateActivityResult, error) {
	if err := validateActivity(in); err != nil {
		return nil, err
	}
	activity, items, err := s.store.CreateActivity(ctx, in)
	if err != nil {
		return nil, wrap(ErrServiceUnavailable, err)
	}
	result := &CreateActivityResult{Activity: activity, Items: make([]ItemReadiness, 0, len(items))}
	for _, item := range items {
		ready := true
		if err := s.gate.SetStockNX(ctx, item.ID, item.AvailableStock); err != nil {
			ready = false
			result.Warnings = append(result.Warnings, fmt.Sprintf("item %d stock not ready: %v", item.ID, err))
		}
		if err := s.gate.SetItemSnapshot(ctx, item, 10*time.Second); err != nil {
			ready = false
			result.Warnings = append(result.Warnings, fmt.Sprintf("item %d snapshot failed: %v", item.ID, err))
		}
		result.Items = append(result.Items, ItemReadiness{Item: item, RedisReady: ready})
	}
	return result, nil
}

func (s *Service) GetStock(ctx context.Context, activityID int64, includeRedis bool) (*StockResult, error) {
	if activityID <= 0 {
		return nil, ErrInvalidArgument
	}
	activity, items, err := s.store.GetActivityStock(ctx, activityID)
	if errors.Is(err, mysqlrepo.ErrNotFound) {
		return nil, ErrItemNotFound
	}
	if err != nil {
		return nil, wrap(ErrServiceUnavailable, err)
	}
	result := &StockResult{Activity: activity, Items: make([]StockItem, 0, len(items))}
	for _, item := range items {
		view := StockItem{Item: item}
		if includeRedis {
			stock, found, err := s.gate.GetStock(ctx, item.ID)
			if err != nil {
				view.RedisError = err.Error()
				result.Warnings = append(result.Warnings, fmt.Sprintf("item %d redis stock: %v", item.ID, err))
			} else if found {
				view.RedisReady = true
				view.RedisStock = &stock
			}
		}
		result.Items = append(result.Items, view)
	}
	return result, nil
}

func (s *Service) PayMock(ctx context.Context, userID int64, orderNo, providerTxnID string, amount int64) (*model.Order, error) {
	if userID <= 0 {
		return nil, ErrUnauthenticated
	}
	if orderNo == "" || providerTxnID == "" || amount < 0 {
		return nil, ErrInvalidArgument
	}
	order, err := s.store.MarkPaid(ctx, userID, orderNo, providerTxnID, amount)
	if errors.Is(err, mysqlrepo.ErrNotFound) {
		return nil, ErrOrderNotFound
	}
	if errors.Is(err, mysqlrepo.ErrPaymentAmount) {
		return nil, ErrPaymentAmount
	}
	if errors.Is(err, mysqlrepo.ErrPayAfterCancel) {
		return nil, ErrPayAfterCancel
	}
	if err != nil {
		return nil, wrap(ErrServiceUnavailable, err)
	}
	return order, nil
}

func (s *Service) CancelExpired(ctx context.Context) (int, error) {
	orders, err := s.store.CancelExpired(ctx, s.cfg.CancelBatch)
	if err != nil {
		return 0, err
	}
	for _, order := range orders {
		code, err := s.gate.Restock(ctx, order.ItemID, order.OrderNo, order.Quantity)
		if err != nil {
			slog.ErrorContext(ctx, "cancel restock failed", "orderNo", order.OrderNo, "itemId", order.ItemID, "error", err)
			continue
		}
		if code != "OK" && code != "ALREADY" {
			slog.WarnContext(ctx, "cancel restock returned non-ok", "orderNo", order.OrderNo, "code", code)
		}
	}
	return len(orders), nil
}

func (s *Service) loadItem(ctx context.Context, itemID int64) (*model.Item, error) {
	item, found, err := s.gate.GetItemSnapshot(ctx, itemID)
	if err != nil {
		return nil, wrap(ErrServiceUnavailable, err)
	}
	if found {
		return item, nil
	}
	item, err = s.store.GetItem(ctx, itemID)
	if errors.Is(err, mysqlrepo.ErrItemNotFound) {
		return nil, ErrItemNotFound
	}
	if err != nil {
		return nil, wrap(ErrServiceUnavailable, err)
	}
	if err := s.gate.SetItemSnapshot(ctx, *item, 10*time.Second); err != nil {
		slog.WarnContext(ctx, "set item snapshot failed", "itemId", itemID, "error", err)
	}
	return item, nil
}

func (s *Service) lookupHistoryGuarded(ctx context.Context, userID int64, key, payloadHash, requestID string) (*SeckillResult, bool, error) {
	if err := s.acquireHistory(ctx); err != nil {
		return nil, false, err
	}
	defer s.releaseHistory()
	return s.lookupHistory(ctx, userID, key, payloadHash, requestID)
}

func (s *Service) lookupHistory(ctx context.Context, userID int64, key, payloadHash, requestID string) (*SeckillResult, bool, error) {
	order, req, err := s.store.LookupRequest(ctx, userID, key)
	if err != nil {
		return nil, false, wrap(ErrServiceUnavailable, err)
	}
	if order != nil {
		if order.PayloadHash != payloadHash {
			return nil, false, wrap(ErrIdempotencyConflict, nil)
		}
		return &SeckillResult{HTTPStatus: 200, Status: "SUCCEEDED", RequestID: requestID, IdempotencyKey: key, Order: order}, true, nil
	}
	if req != nil {
		if req.PayloadHash != payloadHash {
			return nil, false, wrap(ErrIdempotencyConflict, nil)
		}
		switch req.State {
		case "PROCESSING":
			return processingResult(requestID, key), true, nil
		case "REJECTED":
			return &SeckillResult{HTTPStatus: 200, Status: "REJECTED", RequestID: requestID, IdempotencyKey: key, Reason: req.Reason}, true, nil
		case "SUCCEEDED":
			return nil, false, wrap(ErrResultUnknown, fmt.Errorf("request succeeded without order"))
		default:
			return nil, false, wrap(ErrResultUnknown, nil)
		}
	}
	return nil, false, nil
}

func (s *Service) allowQuery(ctx context.Context) (func(), error) {
	allowed, _, err := s.gate.Allow(ctx, s.gate.QueryKey(), s.cfg.QueryRPS, s.cfg.QueryBurst, 1)
	if err == nil {
		if !allowed {
			return nil, wrap(ErrRateLimited, nil)
		}
		return func() {}, nil
	}
	select {
	case s.querySem <- struct{}{}:
		return func() { <-s.querySem }, nil
	case <-ctx.Done():
		return nil, wrap(ErrServiceUnavailable, ctx.Err())
	case <-time.After(250 * time.Millisecond):
		return nil, wrap(ErrRateLimited, nil)
	}
}

func (s *Service) acquireHistory(ctx context.Context) error {
	select {
	case s.querySem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return wrap(ErrServiceUnavailable, ctx.Err())
	case <-time.After(250 * time.Millisecond):
		return wrap(ErrRateLimited, nil)
	}
}

func (s *Service) releaseHistory() { <-s.querySem }

func validateItemWindow(item *model.Item, quantity int) error {
	if item.ActivityStatus != "" && item.ActivityStatus != "ACTIVE" {
		return ErrActivityEnded
	}
	now := time.Now().UTC()
	if !item.ActivityStart.IsZero() && now.Before(item.ActivityStart) {
		return ErrActivityNotStarted
	}
	if !item.ActivityEnd.IsZero() && !now.Before(item.ActivityEnd) {
		return ErrActivityEnded
	}
	if quantity <= 0 || quantity > item.PerUserLimit {
		return ErrLimitExceeded
	}
	return nil
}

func validateActivity(in model.ActivityInput) error {
	if strings.TrimSpace(in.Name) == "" || len(in.Name) > 128 {
		return ErrInvalidArgument
	}
	if in.StartAt.IsZero() || in.EndAt.IsZero() || !in.EndAt.After(in.StartAt) {
		return ErrInvalidArgument
	}
	if len(in.Items) == 0 {
		return ErrInvalidArgument
	}
	seen := make(map[string]struct{}, len(in.Items))
	for _, item := range in.Items {
		if strings.TrimSpace(item.SKUCode) == "" || len(item.SKUCode) > 64 || len(item.Name) > 128 {
			return ErrInvalidArgument
		}
		if _, ok := seen[item.SKUCode]; ok {
			return ErrInvalidArgument
		}
		seen[item.SKUCode] = struct{}{}
		if item.PriceCents < 0 || item.TotalStock <= 0 || item.PerUserLimit <= 0 || item.PerUserLimit > item.TotalStock {
			return ErrInvalidArgument
		}
	}
	return nil
}

func processingResult(requestID, key string) *SeckillResult {
	return &SeckillResult{
		HTTPStatus:     202,
		Status:         "PROCESSING",
		RequestID:      requestID,
		IdempotencyKey: key,
		RetryAfterMs:   500,
	}
}

func PayloadHash(itemID int64, quantity int) string {
	sum := sha256.Sum256([]byte("v1|" + strconv.FormatInt(itemID, 10) + "|" + strconv.Itoa(quantity)))
	return hex.EncodeToString(sum[:])
}

func RequestID(userID int64, key string) string {
	sum := sha256.Sum256([]byte(strconv.FormatInt(userID, 10) + "|" + key))
	return "req_" + hex.EncodeToString(sum[:])[:24]
}

func randomID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
