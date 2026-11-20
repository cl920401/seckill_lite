package worker

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"seckill_lite/internal/mysqlrepo"
	"seckill_lite/internal/redisrepo"
)

type Store interface {
	HandleReservation(ctx context.Context, in mysqlrepo.ReservationInput) (mysqlrepo.ReservationOutcome, error)
	MarkCompensationDone(ctx context.Context, reservationID string) error
}

// Gate 负责消息投递和 Redis 状态；订单是否成功、预约是否需要补偿由 Store 的持久结果决定。
type Gate interface {
	ReadGroup(ctx context.Context, consumer string, count int64, block time.Duration) ([]redisrepo.Message, error)
	AutoClaim(ctx context.Context, consumer string, minIdle time.Duration, count int64) ([]redisrepo.Message, error)
	Ack(ctx context.Context, messageID string) error
	MarkSuccess(ctx context.Context, userID int64, key, reservationID, orderNo string) error
	Compensate(ctx context.Context, itemID int64, userID int64, key, reservationID string, quantity int, reason string) (string, error)
}

type Worker struct {
	store     Store
	gate      Gate
	consumer  string
	claimIdle time.Duration
	batch     int64
	log       *slog.Logger
}

func New(store Store, gate Gate, claimIdle time.Duration) *Worker {
	if claimIdle <= 0 {
		claimIdle = 60 * time.Second
	}
	host, _ := os.Hostname()
	return &Worker{
		store:     store,
		gate:      gate,
		consumer:  fmt.Sprintf("worker-%s-%d", host, os.Getpid()),
		claimIdle: claimIdle,
		batch:     50,
		log:       slog.Default(),
	}
}

// Run 消费新预约，并周期性认领长时间未确认的 Pending 消息。
// 当前批量拉取 50 条后逐条串行处理，批大小不是并发数；退出后未 ACK 的消息留待再次认领。
func (w *Worker) Run(ctx context.Context) {
	claimEvery := w.claimIdle / 2
	if claimEvery < time.Second {
		claimEvery = time.Second
	}
	claimTicker := time.NewTicker(claimEvery)
	defer claimTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-claimTicker.C:
			// 认领用于恢复未完成的投递，不意味着旧消费者一定停止，也不证明上次业务处理失败。
			w.claim(ctx)
		default:
		}

		messages, err := w.gate.ReadGroup(ctx, w.consumer, w.batch, time.Second)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			w.log.ErrorContext(ctx, "read stream group failed", "error", err)
			// 拉取失败固定等待 500ms；当前尚未实现按消息失败次数递增的退避或重试阈值。
			time.Sleep(500 * time.Millisecond)
			continue
		}
		for _, msg := range messages {
			w.processAndAck(ctx, msg, false)
		}
	}
}

// claim 与首次投递走同一处理协议，不能因为消息被重新认领就直接回补库存。
func (w *Worker) claim(ctx context.Context) {
	messages, err := w.gate.AutoClaim(ctx, w.consumer, w.claimIdle, w.batch)
	if err != nil {
		if ctx.Err() == nil {
			w.log.ErrorContext(ctx, "xautoclaim failed", "error", err)
		}
		return
	}
	for _, msg := range messages {
		w.processAndAck(ctx, msg, true)
	}
}

// processAndAck 将业务处理与队列确认分开：只有 Process 确认完成，才允许 XACK。
// 超时、数据库提交结果不明或补偿失败都保留 Pending；日志中的 claimed 仅用于区分投递来源。
func (w *Worker) processAndAck(ctx context.Context, msg redisrepo.Message, claimed bool) {
	processCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	err := w.Process(processCtx, msg)
	cancel()
	if err != nil {
		w.log.ErrorContext(ctx, "process reservation failed; keeping pending",
			"messageId", msg.ID,
			"reservationId", msg.ReservationID,
			"userId", msg.UserID,
			"itemId", msg.ItemID,
			"claimed", claimed,
			"error", err)
		return
	}
	// ACK 失败可能造成重投，正确性依赖持久幂等协议；ACK 也不会删除 Stream 历史条目。
	if err := w.gate.Ack(ctx, msg.ID); err != nil {
		w.log.ErrorContext(ctx, "xack failed", "messageId", msg.ID, "reservationId", msg.ReservationID, "error", err)
	}
}

// Process 按“持久业务裁决 → Redis 结果回填或补偿 → 持久补偿完成”处理一条预约。
// msg.ID 标识消息，ReservationID 标识实际预扣；业务幂等键为 UserID + IdempotencyKey。
func (w *Worker) Process(ctx context.Context, msg redisrepo.Message) error {
	// 先由 MySQL 事务裁决成功、拒绝或额外预约；错误不能直接解释成“没有建单”。
	outcome, err := w.store.HandleReservation(ctx, mysqlrepo.ReservationInput{
		ReservationID:  msg.ReservationID,
		UserID:         msg.UserID,
		IdempotencyKey: msg.IdempotencyKey,
		ItemID:         msg.ItemID,
		Quantity:       msg.Quantity,
		PayloadHash:    msg.PayloadHash,
	})
	if err != nil {
		return fmt.Errorf("handle reservation: %w", err)
	}

	if outcome.Order != nil {
		// 已有成功订单（包括同预约重投）不回补 Redis；回填只是缓存更新，失败仍可查 DB。
		if err := w.gate.MarkSuccess(ctx, msg.UserID, msg.IdempotencyKey, msg.ReservationID, outcome.Order.OrderNo); err != nil {
			w.log.WarnContext(ctx, "mark success cache failed; DB remains authoritative",
				"reservationId", msg.ReservationID, "orderNo", outcome.Order.OrderNo, "error", err)
		}
		return nil
	}

	if outcome.Compensate {
		// 只有 DB 已持久终结且补偿未完成的拒绝/额外预约，才进入 Redis 回补。
		reason := outcome.Reason
		if reason == "" {
			reason = outcome.Kind
		}
		code, err := w.gate.Compensate(ctx, msg.ItemID, msg.UserID, msg.IdempotencyKey, msg.ReservationID, msg.Quantity, reason)
		if err != nil {
			return fmt.Errorf("compensate reservation: %w", err)
		}
		if code == "MISS" {
			return fmt.Errorf("compensate reservation returned MISS; stock key missing, manual review required")
		}
		if code != "OK" && code != "ALREADY" {
			return fmt.Errorf("compensate reservation returned %s", code)
		}
		// Redis 返回 OK/ALREADY 后才记 DONE；若此处失败，重投再次通过预约标记去重。
		// 这依赖 Lua 的标记可信：脚本部分执行后留下的错误标记会让 ALREADY 被误当作完成。
		if err := w.store.MarkCompensationDone(ctx, msg.ReservationID); err != nil {
			return fmt.Errorf("mark compensation done: %w", err)
		}
		return nil
	}

	if outcome.CompensationDone {
		// DB 已记 DONE 时不再调用回补；Redis 标记缺失本身不是再次增加库存的依据。
		return nil
	}

	return fmt.Errorf("reservation outcome has neither order nor compensation state: kind=%s", outcome.Kind)
}
