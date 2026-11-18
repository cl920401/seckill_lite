package worker

import (
	"context"
	"errors"
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
}

func (f *fakeStore) HandleReservation(ctx context.Context, in mysqlrepo.ReservationInput) (mysqlrepo.ReservationOutcome, error) {
	return f.outcome, f.err
}
func (f *fakeStore) MarkCompensationDone(ctx context.Context, reservationID string) error {
	f.doneID = reservationID
	return nil
}

type fakeGate struct {
	compensated    int
	marked         int
	acked          int
	compensateCode string
	compensateErr  error
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
	return nil
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
