package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/jiin-yang/notification-dispatcher/internal/app"
	"github.com/jiin-yang/notification-dispatcher/internal/domain"
)

type fakeProvider struct {
	err   error
	calls int
}

func (p *fakeProvider) Send(_ context.Context, _ domain.Notification) error {
	p.calls++
	return p.err
}

type fakeUpdater struct {
	statuses map[uuid.UUID]domain.Status
	err      error
}

func newFakeUpdater() *fakeUpdater { return &fakeUpdater{statuses: map[uuid.UUID]domain.Status{}} }

func (u *fakeUpdater) UpdateStatus(_ context.Context, id uuid.UUID, s domain.Status) error {
	if u.err != nil {
		return u.err
	}
	u.statuses[id] = s
	return nil
}

func buildMessageBody(t *testing.T, id uuid.UUID) []byte {
	t.Helper()
	body, err := json.Marshal(app.NotificationMessage{
		ID:            id,
		Recipient:     "+905551234567",
		Channel:       domain.ChannelSMS,
		Content:       "hi",
		Priority:      domain.PriorityNormal,
		CorrelationID: uuid.New(),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return body
}

// handle is a helper that calls Handle with the Phase 4 signature, passing
// attempt=0 and empty routing key (the defaults for a first-try message).
func handle(t *testing.T, uc *app.DeliverUseCase, body []byte, corrID string) error {
	t.Helper()
	return uc.Handle(context.Background(), body, corrID, 0, "sms.normal")
}

func TestDeliver_Success(t *testing.T) {
	prov := &fakeProvider{}
	upd := newFakeUpdater()
	uc := app.NewDeliverUseCase(prov, upd, nil)

	id := uuid.New()
	if err := handle(t, uc, buildMessageBody(t, id), "corr"); err != nil {
		t.Fatalf("Handle returned %v", err)
	}
	if prov.calls != 1 {
		t.Errorf("provider.Send calls = %d, want 1", prov.calls)
	}
	if upd.statuses[id] != domain.StatusDelivered {
		t.Errorf("status = %q, want delivered", upd.statuses[id])
	}
}

func TestDeliver_TerminalFailure(t *testing.T) {
	prov := &fakeProvider{}
	prov.err = wrapped{domain.ErrDeliveryFailed}
	upd := newFakeUpdater()
	uc := app.NewDeliverUseCase(prov, upd, nil)

	id := uuid.New()
	err := handle(t, uc, buildMessageBody(t, id), "corr")
	if err != nil {
		t.Fatalf("Handle should swallow terminal error (ack), got %v", err)
	}
	if upd.statuses[id] != domain.StatusFailed {
		t.Errorf("status = %q, want failed", upd.statuses[id])
	}
}

type wrapped struct{ inner error }

func (w wrapped) Error() string { return w.inner.Error() }
func (w wrapped) Unwrap() error { return w.inner }

func TestDeliver_TransientError(t *testing.T) {
	prov := &fakeProvider{err: errors.New("network blip")}
	upd := newFakeUpdater()
	uc := app.NewDeliverUseCase(prov, upd, nil)

	err := handle(t, uc, buildMessageBody(t, uuid.New()), "corr")
	if err == nil {
		t.Fatal("want non-nil error to trigger nack, got nil")
	}
}

func TestDeliver_ContextCanceled_DoesNotMarkFailed(t *testing.T) {
	prov := &fakeProvider{err: context.Canceled}
	upd := newFakeUpdater()
	uc := app.NewDeliverUseCase(prov, upd, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := uc.Handle(ctx, buildMessageBody(t, uuid.New()), "corr", 0, "sms.normal")
	if err == nil {
		t.Fatal("want non-nil error so consumer nacks, got nil")
	}
	if len(upd.statuses) != 0 {
		t.Errorf("status must not be updated on cancellation; got %v", upd.statuses)
	}
}

func TestDeliver_BadBody_NoRetryPublisher(t *testing.T) {
	prov := &fakeProvider{}
	upd := newFakeUpdater()
	uc := app.NewDeliverUseCase(prov, upd, nil)

	// Without a retry publisher, poison messages return an error (nack).
	err := uc.Handle(context.Background(), []byte("not-json"), "corr", 0, "sms.normal")
	if err == nil {
		t.Fatal("want error on bad body when no retry publisher")
	}
	if prov.calls != 0 {
		t.Errorf("provider should not be called on bad body, got %d", prov.calls)
	}
}

// fakeRetryPublisher records calls for unit-test assertions.
type fakeRetryPublisher struct {
	retries []retryCall
	dlqs    []dlqCall
}

type retryCall struct {
	channel  string
	priority string
	level    int
	attempt  int
}

type dlqCall struct {
	channel string
	attempt int
}

func (f *fakeRetryPublisher) PublishRetry(_ context.Context, channel, priority string, level, attempt int, _ string, _ []byte) error {
	f.retries = append(f.retries, retryCall{channel: channel, priority: priority, level: level, attempt: attempt})
	return nil
}

func (f *fakeRetryPublisher) PublishDLQ(_ context.Context, channel string, attempt int, _ string, _ string, _ []byte) error {
	f.dlqs = append(f.dlqs, dlqCall{channel: channel, attempt: attempt})
	return nil
}

func TestDeliver_WithRetryPublisher_RoutesToRetry(t *testing.T) {
	prov := &fakeProvider{err: wrapped{domain.ErrDeliveryFailed}}
	upd := newFakeUpdater()
	rp := &fakeRetryPublisher{}
	uc := app.NewDeliverUseCase(prov, upd, nil).WithRetryPublisher(rp)

	id := uuid.New()
	// attempt=0 → retryLevel=1 ≤ 3, should route to retry
	if err := uc.Handle(context.Background(), buildMessageBody(t, id), "corr", 0, "sms.normal"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rp.retries) != 1 {
		t.Errorf("retry publishes = %d, want 1", len(rp.retries))
	}
	if len(rp.dlqs) != 0 {
		t.Errorf("dlq publishes = %d, want 0", len(rp.dlqs))
	}
	// buildMessageBody uses PriorityNormal → priority must be preserved on retry.
	if got := rp.retries[0].priority; got != "normal" {
		t.Errorf("retry priority = %q, want %q", got, "normal")
	}
	// Status must not be changed to failed (left as pending for retry)
	if _, ok := upd.statuses[id]; ok {
		t.Errorf("status must not be updated on retry, got %v", upd.statuses[id])
	}
}

func TestDeliver_WithRetryPublisher_RoutesToDLQ(t *testing.T) {
	prov := &fakeProvider{err: wrapped{domain.ErrDeliveryFailed}}
	upd := newFakeUpdater()
	rp := &fakeRetryPublisher{}
	uc := app.NewDeliverUseCase(prov, upd, nil).WithRetryPublisher(rp)

	id := uuid.New()
	// attempt=3 → nextAttempt=4 > 3, should route to DLQ
	if err := uc.Handle(context.Background(), buildMessageBody(t, id), "corr", 3, "sms.normal"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rp.retries) != 0 {
		t.Errorf("retry publishes = %d, want 0", len(rp.retries))
	}
	if len(rp.dlqs) != 1 {
		t.Errorf("dlq publishes = %d, want 1", len(rp.dlqs))
	}
	if upd.statuses[id] != domain.StatusFailed {
		t.Errorf("status = %q, want failed on DLQ routing", upd.statuses[id])
	}
}

func TestDeliver_BadBody_WithRetryPublisher(t *testing.T) {
	prov := &fakeProvider{}
	upd := newFakeUpdater()
	rp := &fakeRetryPublisher{}
	uc := app.NewDeliverUseCase(prov, upd, nil).WithRetryPublisher(rp)

	// Poison message with retry publisher → DLQ (ack, no error).
	err := uc.Handle(context.Background(), []byte("not-json"), "corr", 0, "sms.normal")
	if err != nil {
		t.Fatalf("want nil error (ack) for poison with retry publisher, got %v", err)
	}
	if len(rp.dlqs) != 1 {
		t.Errorf("dlq publishes = %d, want 1 for poison message", len(rp.dlqs))
	}
	if prov.calls != 0 {
		t.Errorf("provider should not be called on bad body")
	}
}
