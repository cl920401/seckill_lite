package redisrepo

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"seckill_lite/internal/model"
)

func TestLuaContracts(t *testing.T) {
	if os.Getenv("SECKILL_REDIS_IT") != "1" {
		t.Skip("set SECKILL_REDIS_IT=1 to run redis integration test")
	}
	addr := os.Getenv("SECKILL_IT_REDIS")
	if addr == "" {
		addr = "127.0.0.1:6379"
	}
	rawDB := os.Getenv("SECKILL_IT_REDIS_DB")
	if rawDB == "" {
		t.Fatal("SECKILL_IT_REDIS_DB must be set to a dedicated logical DB (for example 15)")
	}
	redisDB, err := strconv.Atoi(rawDB)
	if err != nil || redisDB < 0 {
		t.Fatalf("invalid SECKILL_IT_REDIS_DB %q", rawDB)
	}
	prefix := fmt.Sprintf("sk:test:%d:", time.Now().UnixNano())
	stream := prefix + "stream:orders"
	g := New(addr, "", redisDB, prefix, stream, "test-workers", time.Hour)
	defer g.Close()
	ctx := context.Background()
	if err := g.EnsureGroup(ctx); err != nil {
		t.Fatal(err)
	}
	itemID := int64(1)
	if err := g.SetStockNX(ctx, itemID, 2); err != nil {
		t.Fatal(err)
	}
	base := ReserveRequest{UserID: 1, Key: "key-12345", PayloadHash: "hash-a", ReservationID: "res-1", ItemID: itemID, Quantity: 1, AcceptedAt: time.Now(), TTL: time.Hour, MaxEntries: 100}
	res, err := g.Reserve(ctx, base)
	if err != nil || res.Code != "ACCEPTED" {
		t.Fatalf("reserve: %+v %v", res, err)
	}
	dup, err := g.Reserve(ctx, base)
	if err != nil || dup.Code != "DUPLICATE" {
		t.Fatalf("duplicate: %+v %v", dup, err)
	}
	conflict := base
	conflict.PayloadHash = "hash-b"
	if got, err := g.Reserve(ctx, conflict); err != nil || got.Code != "CONFLICT" {
		t.Fatalf("conflict: %+v %v", got, err)
	}
	tooMany := base
	tooMany.Key = "key-67890"
	tooMany.Quantity = 3
	if got, err := g.Reserve(ctx, tooMany); err != nil || got.Code != "SOLD_OUT" {
		t.Fatalf("soldout: %+v %v", got, err)
	}
	state, err := g.GetRequest(ctx, 1, base.Key)
	if err != nil || state == nil || state.Status != "PROCESSING" {
		t.Fatalf("state: %+v %v", state, err)
	}
	// Queue capacity is a hard XLEN limit and must not deduct stock.
	queueFull := base
	queueFull.Key = "key-queuefull"
	queueFull.ReservationID = "res-queue"
	queueFull.MaxEntries = 1
	if got, err := g.Reserve(ctx, queueFull); err != nil || got.Code != "QUEUE_FULL" {
		t.Fatalf("queue full: %+v %v", got, err)
	}
	if stock, _, _ := g.GetStock(ctx, itemID); stock != 1 {
		t.Fatalf("stock after queue full=%d, want 1", stock)
	}
	// A stale reservation must not overwrite the winning request state.
	if err := g.MarkSuccess(ctx, 1, base.Key, "wrong-reservation", "SK-BAD"); err != nil {
		t.Fatal(err)
	}
	state, _ = g.GetRequest(ctx, 1, base.Key)
	if state.Status != "PROCESSING" || state.OrderNo != "" {
		t.Fatalf("stale mark success changed state: %+v", state)
	}
	if err := g.MarkSuccess(ctx, 1, base.Key, "res-1", "SK1"); err != nil {
		t.Fatal(err)
	}
	state, _ = g.GetRequest(ctx, 1, base.Key)
	if state.Status != "SUCCEEDED" || state.OrderNo != "SK1" {
		t.Fatalf("success state: %+v", state)
	}
	if code, err := g.Compensate(ctx, itemID, 1, "other-key-123", "res-2", 1, "REJECTED"); err != nil || code != "OK" {
		t.Fatalf("compensate: %s %v", code, err)
	}
	if code, err := g.Compensate(ctx, itemID, 1, "other-key-123", "res-2", 1, "REJECTED"); err != nil || code != "ALREADY" {
		t.Fatalf("compensate again: %s %v", code, err)
	}
	if ttl, err := g.Client().TTL(ctx, prefix+"compensated:res-2").Result(); err != nil || ttl != -1 {
		t.Fatalf("compensation marker ttl=%v err=%v, want no ttl", ttl, err)
	}
	if code, err := g.Compensate(ctx, 999, 1, "missing-stock-key", "res-missing", 1, "REJECTED"); err != nil || code != "MISS" {
		t.Fatalf("compensate missing stock: %s %v", code, err)
	}
	if exists, _ := g.Client().Exists(ctx, prefix+"compensated:res-missing").Result(); exists != 0 {
		t.Fatal("missing stock compensation must not create marker")
	}
	if code, err := g.Restock(ctx, itemID, "SK1", 1); err != nil || code != "OK" {
		t.Fatalf("restock: %s %v", code, err)
	}
	if code, err := g.Restock(ctx, itemID, "SK1", 1); err != nil || code != "ALREADY" {
		t.Fatalf("restock again: %s %v", code, err)
	}
	if ttl, err := g.Client().TTL(ctx, prefix+"restocked:SK1").Result(); err != nil || ttl <= 0 || ttl > 86400*time.Second {
		t.Fatalf("restock marker ttl=%v err=%v", ttl, err)
	}
	if code, err := g.Restock(ctx, 999, "SK-MISSING", 1); err != nil || code != "MISS" {
		t.Fatalf("restock missing stock: %s %v", code, err)
	}
	if exists, _ := g.Client().Exists(ctx, prefix+"restocked:SK-MISSING").Result(); exists != 0 {
		t.Fatal("missing stock restock must not create marker")
	}
	stock, found, err := g.GetStock(ctx, itemID)
	if err != nil || !found || stock != 3 {
		t.Fatalf("stock=%d found=%v err=%v", stock, found, err)
	}
	allowed, _, err := g.Allow(ctx, prefix+"rate", 1, 1, 1)
	if err != nil || !allowed {
		t.Fatalf("first rate: %v %v", allowed, err)
	}
	allowed, _, err = g.Allow(ctx, prefix+"rate", 1, 1, 1)
	if err != nil || allowed {
		t.Fatalf("second rate should be limited: %v %v", allowed, err)
	}
	if err := g.SetItemSnapshot(ctx, model.Item{ID: 9, ActivityID: 1, PriceCents: 1, TotalStock: 1, PerUserLimit: 1}, time.Minute); err != nil {
		t.Fatal(err)
	}
	if item, ok, err := g.GetItemSnapshot(ctx, 9); err != nil || !ok || item.TotalStock != 1 {
		t.Fatalf("snapshot: %+v %v %v", item, ok, err)
	}
	keys, _ := g.Client().Keys(ctx, prefix+"*").Result()
	if len(keys) > 0 {
		_ = g.Client().Del(ctx, keys...).Err()
	}
}
