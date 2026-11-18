package redisrepo

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	luaembed "seckill_lite/internal/lua"
	"seckill_lite/internal/model"
)

var (
	reserveScript     *redis.Script
	compensateScript  *redis.Script
	restockScript     *redis.Script
	markSuccessScript *redis.Script
	rateLimitScript   *redis.Script
)

func init() {
	mustLoad := func(name string) *redis.Script {
		b, err := luaembed.FS.ReadFile(name)
		if err != nil {
			panic(err)
		}
		return redis.NewScript(string(b))
	}
	reserveScript = mustLoad("reserve.lua")
	compensateScript = mustLoad("compensate.lua")
	restockScript = mustLoad("restock.lua")
	markSuccessScript = mustLoad("mark_success.lua")
	rateLimitScript = mustLoad("rate_limit.lua")
}

type Gateway struct {
	client     *redis.Client
	prefix     string
	stream     string
	group      string
	restockTTL time.Duration
}

type ReserveRequest struct {
	UserID        int64
	Key           string
	PayloadHash   string
	ReservationID string
	ItemID        int64
	Quantity      int
	AcceptedAt    time.Time
	TTL           time.Duration
	MaxEntries    int64
}

type ReserveResult struct {
	Code      string
	Status    string
	OrderNo   string
	Remaining string
	MessageID string
}

type RequestState struct {
	Status        string
	ReservationID string
	PayloadHash   string
	OrderNo       string
	Reason        string
	AcceptedAt    int64
	UpdatedAt     int64
}

type Message struct {
	ID             string
	ReservationID  string
	IdempotencyKey string
	UserID         int64
	ItemID         int64
	Quantity       int
	PayloadHash    string
	AcceptedAt     int64
}

func New(addr, password string, db int, prefix, stream, group string, restockTTL time.Duration) *Gateway {
	if prefix == "" {
		prefix = "sk:"
	}
	return &Gateway{
		client: redis.NewClient(&redis.Options{
			Addr:     addr,
			Password: password,
			DB:       db,
		}),
		prefix:     prefix,
		stream:     stream,
		group:      group,
		restockTTL: restockTTL,
	}
}

func (g *Gateway) Client() *redis.Client { return g.client }

// Prefix returns the configured key prefix. It is exposed so callers (for
// example, tests or operations tooling) can address marker keys that are not
// otherwise surfaced by the Gateway.
func (g *Gateway) Prefix() string { return g.prefix }

func (g *Gateway) Close() error { return g.client.Close() }

func (g *Gateway) Ping(ctx context.Context) error { return g.client.Ping(ctx).Err() }

func (g *Gateway) StockKey(itemID int64) string {
	return fmt.Sprintf("%sstock:%d", g.prefix, itemID)
}

func (g *Gateway) ReqKey(userID int64, key string) string {
	return fmt.Sprintf("%sreq:%d:%s", g.prefix, userID, key)
}

func (g *Gateway) ItemSnapshotKey(itemID int64) string {
	return fmt.Sprintf("%sitem:%d", g.prefix, itemID)
}

func (g *Gateway) IngressKey() string { return g.prefix + "rl:ingress:global" }
func (g *Gateway) QueryKey() string   { return g.prefix + "rl:query:global" }
func (g *Gateway) UserKey(userID int64) string {
	return fmt.Sprintf("%srl:user:%d", g.prefix, userID)
}

func (g *Gateway) Allow(ctx context.Context, key string, rate float64, burst, cost int) (bool, int, error) {
	if rate <= 0 || burst <= 0 {
		return false, 0, fmt.Errorf("invalid rate limit configuration")
	}
	now := time.Now().UnixMilli()
	res, err := rateLimitScript.Run(ctx, g.client, []string{key},
		strconv.FormatFloat(rate, 'f', -1, 64),
		burst, now, cost, int64(10*time.Second/time.Millisecond),
	).Result()
	if err != nil {
		return false, 0, fmt.Errorf("rate limit script: %w", err)
	}
	values, ok := res.([]interface{})
	if !ok || len(values) < 3 {
		return false, 0, fmt.Errorf("unexpected rate limit response")
	}
	allowed := asInt(values[0]) == 1
	retry := int(asInt(values[2]))
	return allowed, retry, nil
}

func (g *Gateway) Reserve(ctx context.Context, req ReserveRequest) (ReserveResult, error) {
	if req.TTL <= 0 {
		req.TTL = 24 * time.Hour
	}
	maxEntries := req.MaxEntries
	if maxEntries <= 0 {
		maxEntries = 10000
	}
	acceptedAt := req.AcceptedAt.UnixMilli()
	res, err := reserveScript.Run(ctx, g.client,
		[]string{g.StockKey(req.ItemID), g.ReqKey(req.UserID, req.Key), g.stream},
		req.UserID, req.Key, req.PayloadHash, req.ReservationID, req.ItemID,
		req.Quantity, acceptedAt, int64(req.TTL/time.Millisecond), maxEntries,
	).Result()
	if err != nil {
		return ReserveResult{}, fmt.Errorf("reserve script: %w", err)
	}
	values, ok := res.([]interface{})
	if !ok || len(values) == 0 {
		return ReserveResult{}, fmt.Errorf("unexpected reserve response")
	}
	out := ReserveResult{Code: asString(values[0])}
	if len(values) > 1 {
		out.Status = asString(values[1])
	}
	if len(values) > 2 {
		out.OrderNo = asString(values[2])
	}
	if out.Code == "ACCEPTED" {
		if len(values) > 1 {
			out.Remaining = asString(values[1])
		}
		if len(values) > 2 {
			out.MessageID = asString(values[2])
		}
	}
	return out, nil
}

func (g *Gateway) GetRequest(ctx context.Context, userID int64, key string) (*RequestState, error) {
	values, err := g.client.HGetAll(ctx, g.ReqKey(userID, key)).Result()
	if err != nil {
		return nil, fmt.Errorf("get request state: %w", err)
	}
	if len(values) == 0 {
		return nil, nil
	}
	return &RequestState{
		Status:        values["status"],
		ReservationID: values["reservation_id"],
		PayloadHash:   values["payload_hash"],
		OrderNo:       values["order_no"],
		Reason:        values["reason"],
		AcceptedAt:    parseInt64(values["accepted_at"]),
		UpdatedAt:     parseInt64(values["updated_at"]),
	}, nil
}

func (g *Gateway) MarkSuccess(ctx context.Context, userID int64, key, reservationID, orderNo string) error {
	_, err := markSuccessScript.Run(ctx, g.client, []string{g.ReqKey(userID, key)},
		reservationID, orderNo, time.Now().UnixMilli()).Result()
	if err != nil {
		return fmt.Errorf("mark success: %w", err)
	}
	return nil
}

func (g *Gateway) Compensate(ctx context.Context, itemID int64, userID int64, key, reservationID string, quantity int, reason string) (string, error) {
	res, err := compensateScript.Run(ctx, g.client,
		[]string{g.StockKey(itemID), g.prefix + "compensated:" + reservationID, g.ReqKey(userID, key)},
		reservationID, quantity, reason, time.Now().UnixMilli()).Result()
	if err != nil {
		return "", fmt.Errorf("compensate script: %w", err)
	}
	values, ok := res.([]interface{})
	if !ok || len(values) == 0 {
		return "", fmt.Errorf("unexpected compensate response")
	}
	return asString(values[0]), nil
}

func (g *Gateway) Restock(ctx context.Context, itemID int64, orderNo string, quantity int) (string, error) {
	ttlSeconds := int64(g.restockTTL / time.Second)
	if ttlSeconds <= 0 {
		ttlSeconds = 86400
	}
	res, err := restockScript.Run(ctx, g.client,
		[]string{g.StockKey(itemID), g.prefix + "restocked:" + orderNo},
		quantity, ttlSeconds).Result()
	if err != nil {
		return "", fmt.Errorf("restock script: %w", err)
	}
	values, ok := res.([]interface{})
	if !ok || len(values) == 0 {
		return "", fmt.Errorf("unexpected restock response")
	}
	return asString(values[0]), nil
}

func (g *Gateway) GetStock(ctx context.Context, itemID int64) (int64, bool, error) {
	v, err := g.client.Get(ctx, g.StockKey(itemID)).Int64()
	if errors.Is(err, redis.Nil) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("get stock: %w", err)
	}
	return v, true, nil
}

func (g *Gateway) SetStockNX(ctx context.Context, itemID int64, stock int) error {
	ok, err := g.client.SetNX(ctx, g.StockKey(itemID), stock, 0).Result()
	if err != nil {
		return fmt.Errorf("set stock nx: %w", err)
	}
	if !ok {
		return fmt.Errorf("stock key already exists")
	}
	return nil
}

func (g *Gateway) SetItemSnapshot(ctx context.Context, item model.Item, ttl time.Duration) error {
	key := g.ItemSnapshotKey(item.ID)
	if ttl <= 0 {
		ttl = 10 * time.Second
	}
	args := map[string]interface{}{
		"activity_id":     item.ActivityID,
		"start_at":        item.ActivityStart.UnixMilli(),
		"end_at":          item.ActivityEnd.UnixMilli(),
		"activity_status": item.ActivityStatus,
		"price_cents":     item.PriceCents,
		"per_user_limit":  item.PerUserLimit,
		"total_stock":     item.TotalStock,
		"updated_at":      time.Now().UnixMilli(),
	}
	if err := g.client.HSet(ctx, key, args).Err(); err != nil {
		return fmt.Errorf("set item snapshot: %w", err)
	}
	if err := g.client.Expire(ctx, key, ttl).Err(); err != nil {
		return fmt.Errorf("expire item snapshot: %w", err)
	}
	return nil
}

func (g *Gateway) GetItemSnapshot(ctx context.Context, itemID int64) (*model.Item, bool, error) {
	values, err := g.client.HGetAll(ctx, g.ItemSnapshotKey(itemID)).Result()
	if err != nil {
		return nil, false, fmt.Errorf("get item snapshot: %w", err)
	}
	if len(values) == 0 {
		return nil, false, nil
	}
	item := &model.Item{
		ID:             itemID,
		ActivityID:     parseInt64(values["activity_id"]),
		PriceCents:     parseInt64(values["price_cents"]),
		TotalStock:     int(parseInt64(values["total_stock"])),
		PerUserLimit:   int(parseInt64(values["per_user_limit"])),
		ActivityStart:  time.UnixMilli(parseInt64(values["start_at"])).UTC(),
		ActivityEnd:    time.UnixMilli(parseInt64(values["end_at"])).UTC(),
		ActivityStatus: values["activity_status"],
	}
	return item, true, nil
}

func (g *Gateway) EnsureGroup(ctx context.Context) error {
	err := g.client.XGroupCreateMkStream(ctx, g.stream, g.group, "0").Err()
	if err != nil && !strings.Contains(err.Error(), "BUSYGROUP") {
		return fmt.Errorf("create stream group: %w", err)
	}
	return nil
}

func (g *Gateway) GroupExists(ctx context.Context) (bool, error) {
	groups, err := g.client.XInfoGroups(ctx, g.stream).Result()
	if err != nil {
		if strings.Contains(err.Error(), "no such key") {
			return false, nil
		}
		return false, fmt.Errorf("xinfo groups: %w", err)
	}
	for _, group := range groups {
		if group.Name == g.group {
			return true, nil
		}
	}
	return false, nil
}

func (g *Gateway) ReadGroup(ctx context.Context, consumer string, count int64, block time.Duration) ([]Message, error) {
	streams, err := g.client.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    g.group,
		Consumer: consumer,
		Streams:  []string{g.stream, ">"},
		Count:    count,
		Block:    block,
	}).Result()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("xreadgroup: %w", err)
	}
	var out []Message
	for _, stream := range streams {
		for _, msg := range stream.Messages {
			parsed, err := parseMessage(msg)
			if err != nil {
				return nil, err
			}
			out = append(out, parsed)
		}
	}
	return out, nil
}

func (g *Gateway) AutoClaim(ctx context.Context, consumer string, minIdle time.Duration, count int64) ([]Message, error) {
	msgs, _, err := g.client.XAutoClaim(ctx, &redis.XAutoClaimArgs{
		Stream:   g.stream,
		Group:    g.group,
		Consumer: consumer,
		MinIdle:  minIdle,
		Start:    "0-0",
		Count:    count,
	}).Result()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("xautoclaim: %w", err)
	}
	out := make([]Message, 0, len(msgs))
	for _, msg := range msgs {
		parsed, err := parseMessage(msg)
		if err != nil {
			return nil, err
		}
		out = append(out, parsed)
	}
	return out, nil
}

func (g *Gateway) Ack(ctx context.Context, messageID string) error {
	if err := g.client.XAck(ctx, g.stream, g.group, messageID).Err(); err != nil {
		return fmt.Errorf("xack: %w", err)
	}
	return nil
}

func (g *Gateway) StreamLen(ctx context.Context) (int64, error) {
	n, err := g.client.XLen(ctx, g.stream).Result()
	if err != nil {
		return 0, fmt.Errorf("xlen: %w", err)
	}
	return n, nil
}

func parseMessage(msg redis.XMessage) (Message, error) {
	field := func(name string) string {
		if v, ok := msg.Values[name]; ok {
			return fmt.Sprint(v)
		}
		return ""
	}
	userID := parseInt64(field("userId"))
	itemID := parseInt64(field("itemId"))
	quantity, err := strconv.Atoi(field("quantity"))
	if err != nil || userID <= 0 || itemID <= 0 || quantity <= 0 {
		return Message{}, fmt.Errorf("invalid stream message %s", msg.ID)
	}
	return Message{
		ID:             msg.ID,
		ReservationID:  field("reservationId"),
		IdempotencyKey: field("idempotencyKey"),
		UserID:         userID,
		ItemID:         itemID,
		Quantity:       quantity,
		PayloadHash:    field("payloadHash"),
		AcceptedAt:     parseInt64(field("acceptedAt")),
	}, nil
}

func asString(v interface{}) string {
	switch x := v.(type) {
	case string:
		return x
	case []byte:
		return string(x)
	default:
		return fmt.Sprint(x)
	}
}

func asInt(v interface{}) int64 {
	return parseInt64(asString(v))
}

func parseInt64(v string) int64 {
	n, _ := strconv.ParseInt(v, 10, 64)
	return n
}
