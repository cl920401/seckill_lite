package worker

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"seckill_lite/internal/model"
	"seckill_lite/internal/mysqlrepo"
	"seckill_lite/internal/redisrepo"
)

type fakeStore struct {
	outcome mysqlrepo.ReservationOutcome
	err     error
	doneID  string
	doneErr error
}

func (f *fakeStore) HandleReservation(ctx context.Context, in mysqlrepo.ReservationInput) (mysqlrepo.ReservationOutcome, error) {
	return f.outcome, f.err
}
func (f *fakeStore) MarkCompensationDone(ctx context.Context, reservationID string) error {
	f.doneID = reservationID
	return f.doneErr
}

type fakeGate struct {
	compensated    int
	marked         int
	acked          int
	compensateCode string
	compensateErr  error
	markErr        error
}

func (f *fakeGate) ReadGroup(ctx context.Context, consumer string, count int64, block time.Duration) ([]redisrepo.Message, error) {
	return nil, nil
}
func (f *fakeGate) AutoClaim(ctx context.Context, consumer string, minIdle time.Duration, count int64) ([]redisrepo.Message, error) {
	return nil, nil
}
func (f *fakeGate) Ack(ctx context.Context, messageID string) error {
	f.acked++
	return nil
}
func (f *fakeGate) MarkSuccess(ctx context.Context, userID int64, key, reservationID, orderNo string) error {
	f.marked++
	return f.markErr
}
func (f *fakeGate) Compensate(ctx context.Context, itemID int64, userID int64, key, reservationID string, quantity int, reason string) (string, error) {
	f.compensated++
	if f.compensateErr != nil {
		return "", f.compensateErr
	}
	if f.compensateCode != "" {
		return f.compensateCode, nil
	}
	return "OK", nil
}

func testMessage() redisrepo.Message {
	return redisrepo.Message{
		ID: "1-0", ReservationID: "res-1", IdempotencyKey: "key-12345",
		UserID: 1, ItemID: 1, Quantity: 1, PayloadHash: "hash",
	}
}

func TestProcessDONEOnlyAcks(t *testing.T) {
	store := &fakeStore{outcome: mysqlrepo.ReservationOutcome{CompensationDone: true}}
	gate := &fakeGate{}
	w := New(store, gate, time.Hour)
	w.processAndAck(context.Background(), testMessage(), false)
	if gate.compensated != 0 {
		t.Fatalf("compensated=%d, want 0", gate.compensated)
	}
	if gate.acked != 1 {
		t.Fatalf("acked=%d, want 1", gate.acked)
	}
	if store.doneID != "" {
		t.Fatalf("unexpected compensation done update: %s", store.doneID)
	}
}

func TestProcessPendingCompensatesAndMarksDone(t *testing.T) {
	store := &fakeStore{outcome: mysqlrepo.ReservationOutcome{
		Kind: "REJECTED", Reason: "SOLD_OUT", Compensate: true,
	}}
	gate := &fakeGate{}
	w := New(store, gate, time.Hour)
	if err := w.Process(context.Background(), testMessage()); err != nil {
		t.Fatal(err)
	}
	if gate.compensated != 1 || store.doneID != "res-1" {
		t.Fatalf("compensated=%d done=%q", gate.compensated, store.doneID)
	}
}

func TestProcessSuccessMarksCache(t *testing.T) {
	order := &model.Order{OrderNo: "SK1"}
	store := &fakeStore{outcome: mysqlrepo.ReservationOutcome{
		Kind: "SUCCESS", Order: order,
	}}
	gate := &fakeGate{}
	w := New(store, gate, time.Hour)
	w.processAndAck(context.Background(), testMessage(), false)
	if gate.marked != 1 || gate.compensated != 0 {
		t.Fatalf("marked=%d compensated=%d", gate.marked, gate.compensated)
	}
	if gate.acked != 1 {
		t.Fatalf("acked=%d, want 1", gate.acked)
	}
}

func TestProcessCompensationMISSKeepsPending(t *testing.T) {
	store := &fakeStore{outcome: mysqlrepo.ReservationOutcome{
		Kind: "REJECTED", Reason: "SOLD_OUT", Compensate: true,
	}}
	gate := &fakeGate{compensateCode: "MISS"}
	w := New(store, gate, time.Hour)
	if err := w.Process(context.Background(), testMessage()); err == nil {
		t.Fatal("expected MISS to keep message pending")
	}
	if store.doneID != "" {
		t.Fatalf("compensation must not be marked DONE: %s", store.doneID)
	}
}

func TestProcessStoreError(t *testing.T) {
	store := &fakeStore{err: errors.New("mysql down")}
	w := New(store, &fakeGate{}, time.Hour)
	if err := w.Process(context.Background(), testMessage()); err == nil {
		t.Fatal("expected store error")
	}
}

func TestProcessUnknownOutcome(t *testing.T) {
	store := &fakeStore{outcome: mysqlrepo.ReservationOutcome{Kind: "UNKNOWN"}}
	w := New(store, &fakeGate{}, time.Hour)
	if err := w.Process(context.Background(), testMessage()); err == nil {
		t.Fatal("expected unknown outcome error")
	}
}

// A compensation script error must keep the reservation pending: it is not
// evidence that stock was restored, so DONE must not be recorded.
func TestProcessCompensationErrorKeepsPending(t *testing.T) {
	store := &fakeStore{outcome: mysqlrepo.ReservationOutcome{
		Kind: "REJECTED", Reason: "SOLD_OUT", Compensate: true,
	}}
	gate := &fakeGate{compensateErr: errors.New("lua error")}
	w := New(store, gate, time.Hour)
	if err := w.Process(context.Background(), testMessage()); err == nil {
		t.Fatal("expected compensation error")
	}
	if store.doneID != "" {
		t.Fatalf("compensation error must not mark DONE: %s", store.doneID)
	}
	if gate.acked != 0 {
		t.Fatalf("failed compensation must not be acked")
	}
}

func TestProcessMarkDoneErrorKeepsPending(t *testing.T) {
	store := &fakeStore{
		outcome: mysqlrepo.ReservationOutcome{Kind: "REJECTED", Reason: "SOLD_OUT", Compensate: true},
		doneErr: errors.New("mysql down"),
	}
	gate := &fakeGate{}
	w := New(store, gate, time.Hour)
	w.processAndAck(context.Background(), testMessage(), false)
	if gate.acked != 0 {
		t.Fatal("message must stay pending when DONE cannot be persisted")
	}
}

// The Redis success cache is not authoritative. If MarkSuccess fails, the
// committed order still stands and the message may be ACKed.
func TestProcessMarkSuccessCacheFailureStillAcks(t *testing.T) {
	store := &fakeStore{outcome: mysqlrepo.ReservationOutcome{
		Kind: "SUCCESS", Order: &model.Order{OrderNo: "SK1"},
	}}
	gate := &fakeGate{markErr: errors.New("redis down")}
	w := New(store, gate, time.Hour)
	w.processAndAck(context.Background(), testMessage(), false)
	if gate.marked != 1 {
		t.Fatalf("mark attempts=%d, want 1", gate.marked)
	}
	if gate.acked != 1 {
		t.Fatalf("committed order must still be acked when cache update fails")
	}
}

type captureHandler struct{ records []string }

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.records = append(h.records, r.Message)
	return nil
}
func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

// A bad message must be retained (not ACKed) and produce an alert log; this
// MVP has no dead-letter system and does not introduce one for tests.
func TestProcessFailureLogsAndKeepsPending(t *testing.T) {
	store := &fakeStore{err: errors.New("mysql down")}
	gate := &fakeGate{}
	w := New(store, gate, time.Hour)
	h := &captureHandler{}
	w.log = slog.New(h)

	w.processAndAck(context.Background(), testMessage(), false)

	if gate.acked != 0 {
		t.Fatal("failed message must not be acked")
	}
	if len(h.records) == 0 || !strings.Contains(h.records[0], "keeping pending") {
		t.Fatalf("expected a keeping-pending alert log, got %v", h.records)
	}
}
