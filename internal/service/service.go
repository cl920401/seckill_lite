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

// Service 的 querySem 让幂等回源与 Redis 故障时的查询共用本进程并发额度，保护 MySQL。
// 它不跨实例共享，也不保存订单或库存等业务事实。
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

// Seckill 负责准入和异步受理，真正的建单与最终库存扣减由 Worker 在 MySQL 事务中完成。
// 阶段顺序不能随意调整：先保护入口，再找历史结果，最后才校验和预扣新请求。
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

	// 全局限流包含幂等回放，否则攻击者可以用同一键反复触发数据库点查。
	// 返回 429 只说明本次调用受限，并不能据此判断同键的先前请求失败。
	allowed, _, limitErr := s.gate.Allow(ctx, s.gate.IngressKey(), s.cfg.IngressRPS, s.cfg.IngressBurst, 1)
	if limitErr != nil {
		// Redis 故障时仍尝试受控读取历史结果，但绝不降级成绕过 Redis 新建订单。
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
		// MySQL 历史结果优先于 Redis 缓存；先回源，再判断缓存指纹是否冲突。
		// 这样陈旧或被污染的缓存不会覆盖已经持久化的订单结果。
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

	// Redis miss 可能只是缓存过期，不能直接当成新购买；活动结束和售罄也不能抹掉历史。
	// 此处 DB 查询失败就停止准入，但查询成功并不保证后续处理期间 DB 始终可用。
	if res, found, err := s.lookupHistoryGuarded(ctx, userID, idempotencyKey, payloadHash, requestID); err != nil {
		return nil, err
	} else if found {
		return res, nil
	}

	// 预检未找到历史，才消耗用户的新单令牌；已命中的回放不会走到这里。
	// 并发同键请求仍可能同时通过预检，因此一部分会被限流，最终去重还要靠 Lua 和 DB。
	allowed, _, err := s.gate.Allow(ctx, s.gate.UserKey(userID), s.cfg.UserRPS, s.cfg.UserBurst, 1)
	if err != nil {
		return nil, wrap(ErrServiceUnavailable, err)
	}
	if !allowed {
		return nil, wrap(ErrRateLimited, nil)
	}

	// 商品快照用于活动窗口和单次购买数量预检，累计限购与最终库存仍由 DB 事务校验。
	item, err := s.loadItem(ctx, itemID)
	if err != nil {
		return nil, err
	}
	if err := validateItemWindow(item, quantity); err != nil {
		return nil, err
	}

	// 每次预扣尝试生成独立预约号，便于区分同一预约重投与同业务键的多余预扣。
	// 若 Lua 发现重复，本次新生成的预约号不会成为已受理预约。
	reservationID, err := randomID()
	if err != nil {
		return nil, wrap(ErrServiceUnavailable, err)
	}
	// Lua 再查幂等并检查库存/队列上限，然后预扣、写 Stream 和 PROCESSING 状态。
	// 脚本执行期间不被其他命令插入，但运行时错误并不提供事务式回滚。
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
		// 网络超时可能发生在脚本已执行之后；503 不等于肯定未受理。
		// 此处不回补库存，客户端应保留原幂等键查询或重试，而不是换键再买。
		return nil, wrap(ErrServiceUnavailable, err)
	}
	switch reserveResult.Code {
	case "ACCEPTED":
		return processingResult(requestID, idempotencyKey), nil
	case "DUPLICATE":
		// 预检与 Lua 之间可能已有 Worker 完成订单，再查持久结果以返回最新终态。
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
		// Lua 所见的指纹同样可能陈旧；有 MySQL 历史时，以持久结果判断是否冲突。
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

// GetRequestStatus 查询购买意图的结果；查不到记录不等于已确认失败，不能据此释放库存。
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
	// 先读 MySQL，避免 Redis 的 PROCESSING 旧状态掩盖已落库订单或持久拒绝结果。
	order, req, err := s.store.LookupRequest(ctx, userID, key)
	if err != nil {
		return nil, wrap(ErrServiceUnavailable, err)
	}
	requestID := RequestID(userID, key)
	if order != nil {
		// SUCCEEDED 指建单成功；是否待支付、已支付或已取消，要看返回订单的 Status。
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
		// 双端无记录既可能是从未受理，也可能是 Redis 数据丢失，因此返回结果未知。
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

// CreateActivity 先持久化活动，再初始化 Redis 准入数据；两步不是一个跨系统事务。
func (s *Service) CreateActivity(ctx context.Context, in model.ActivityInput) (*CreateActivityResult, error) {
	if err := validateActivity(in); err != nil {
		return nil, err
	}
	activity, items, err := s.store.CreateActivity(ctx, in)
	if err != nil {
		return nil, wrap(ErrServiceUnavailable, err)
	}
	// 到这里活动已在 DB 提交。Redis 失败通过 redisReady/warnings 暴露，不撤销已创建活动。
	result := &CreateActivityResult{Activity: activity, Items: make([]ItemReadiness, 0, len(items))}
	for _, item := range items {
		ready := true
		// 仅对本次新建商品使用 NX，避免覆盖已有预扣；这不是重启后的库存重建入口。
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

// PayMock 只模拟支付结果。重复支付、金额校验和支付/取消竞争交给仓储事务处理，
// 服务层映射业务错误；这里不调用真实支付渠道，也没有自动对账或退款。
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

// CancelExpired 只取消已落库且过期的待支付订单，不处理“预扣了但查不到订单”的预约。
func (s *Service) CancelExpired(ctx context.Context) (int, error) {
	orders, err := s.store.CancelExpired(ctx, s.cfg.CancelBatch)
	if err != nil {
		return 0, err
	}
	// 返回的订单已提交取消、DB 库存/限购回补和 CANCEL 流水，随后才尽力回补 Redis。
	// DB 与 Redis 之间存在失败窗口；当前实现只告警，没有持久化重试任务或自动修复。
	for _, order := range orders {
		// Redis 按订单号标记去重；标记过期或丢失不能单独作为人工再次回补的依据。
		code, err := s.gate.Restock(ctx, order.ItemID, order.OrderNo, order.Quantity)
		if err != nil {
			slog.ErrorContext(ctx, "cancel restock failed", "orderNo", order.OrderNo, "itemId", order.ItemID, "error", err)
			continue
		}
		if code != "OK" && code != "ALREADY" {
			slog.WarnContext(ctx, "cancel restock returned non-ok", "orderNo", order.OrderNo, "code", code)
		}
	}
	// 数量表示 DB 已取消的订单数，不表示 Redis 回补全部成功。
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

// lookupHistoryGuarded 在每次幂等回源前占用本地额度，Redis 正常时也使用这层保护。
// 额度只约束并发查询，不锁定业务键；订单幂等性仍依赖后续 Lua 和持久事务协议。
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
			// 持久请求称成功却没有订单，不能当成新单重做，更不能猜测应当回补。
			return nil, false, wrap(ErrResultUnknown, fmt.Errorf("request succeeded without order"))
		default:
			return nil, false, wrap(ErrResultUnknown, nil)
		}
	}
	return nil, false, nil
}

// allowQuery 正常情况下用 Redis 全局限流；Redis 不可用时允许受限的 DB 历史查询。
// 本地信号量只在故障分支生效，多个实例各有额度，不能冒充故障时的全局限流。
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

// acquireHistory 最多等待 250ms 获取本地额度，并响应调用方取消，避免无限等待。
// 此处只限制等额度的时间；真正 DB 调用的截止时间仍由传入的 ctx 等机制控制。
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

// processingResult 的 202 只表示受理/处理中，不保证最终建单；客户端按原键继续查询。
func processingResult(requestID, key string) *SeckillResult {
	return &SeckillResult{
		HTTPStatus:     202,
		Status:         "PROCESSING",
		RequestID:      requestID,
		IdempotencyKey: key,
		RetryAfterMs:   500,
	}
}

// PayloadHash 描述购买内容，用来识别同一幂等键是否被换成了另一份请求。
// 用户隔离由业务键中的 userID 实现；指纹不是预约号，也不承担请求身份标识。
func PayloadHash(itemID int64, quantity int) string {
	sum := sha256.Sum256([]byte("v1|" + strconv.FormatInt(itemID, 10) + "|" + strconv.Itoa(quantity)))
	return hex.EncodeToString(sum[:])
}

// RequestID 是用户与幂等键派生的稳定展示标识，同键重试不变；它不包含购买内容。
// 业务幂等使用完整用户/幂等键及 payloadHash 校验；reservationID 则用于区分具体预约。
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
