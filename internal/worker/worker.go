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
			w.claim(ctx)
		default:
		}

		messages, err := w.gate.ReadGroup(ctx, w.consumer, w.batch, time.Second)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			w.log.ErrorContext(ctx, "read stream group failed", "error", err)
			time.Sleep(500 * time.Millisecond)
			continue
		}
		for _, msg := range messages {
			w.processAndAck(ctx, msg, false)
		}
	}
}

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
	if err := w.gate.Ack(ctx, msg.ID); err != nil {
		w.log.ErrorContext(ctx, "xack failed", "messageId", msg.ID, "reservationId", msg.ReservationID, "error", err)
	}
}

func (w *Worker) Process(ctx context.Context, msg redisrepo.Message) error {
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
		if err := w.gate.MarkSuccess(ctx, msg.UserID, msg.IdempotencyKey, msg.ReservationID, outcome.Order.OrderNo); err != nil {
			w.log.WarnContext(ctx, "mark success cache failed; DB remains authoritative",
				"reservationId", msg.ReservationID, "orderNo", outcome.Order.OrderNo, "error", err)
		}
		return nil
	}

	if outcome.Compensate {
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
		if err := w.store.MarkCompensationDone(ctx, msg.ReservationID); err != nil {
			return fmt.Errorf("mark compensation done: %w", err)
		}
		return nil
	}

	if outcome.CompensationDone {
		return nil
	}

	return fmt.Errorf("reservation outcome has neither order nor compensation state: kind=%s", outcome.Kind)
}
