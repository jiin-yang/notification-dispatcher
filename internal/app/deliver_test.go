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

func TestDeliver_Success(t *testing.T) {
	prov := &fakeProvider{}
	upd := newFakeUpdater()
	uc := app.NewDeliverUseCase(prov, upd, nil)

	id := uuid.New()
	if err := uc.Handle(context.Background(), buildMessageBody(t, id), "corr"); err != nil {
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
	prov := &fakeProvider{err: errors.New("wrap: " + domain.ErrDeliveryFailed.Error())}
	// Use a wrapped sentinel so errors.Is works.
	prov.err = wrapped{domain.ErrDeliveryFailed}
	upd := newFakeUpdater()
	uc := app.NewDeliverUseCase(prov, upd, nil)

	id := uuid.New()
	err := uc.Handle(context.Background(), buildMessageBody(t, id), "corr")
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

	err := uc.Handle(context.Background(), buildMessageBody(t, uuid.New()), "corr")
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

	err := uc.Handle(ctx, buildMessageBody(t, uuid.New()), "corr")
	if err == nil {
		t.Fatal("want non-nil error so consumer nacks, got nil")
	}
	if len(upd.statuses) != 0 {
		t.Errorf("status must not be updated on cancellation; got %v", upd.statuses)
	}
}

func TestDeliver_BadBody(t *testing.T) {
	prov := &fakeProvider{}
	upd := newFakeUpdater()
	uc := app.NewDeliverUseCase(prov, upd, nil)

	err := uc.Handle(context.Background(), []byte("not-json"), "corr")
	if err == nil {
		t.Fatal("want error on bad body")
	}
	if prov.calls != 0 {
		t.Errorf("provider should not be called on bad body, got %d", prov.calls)
	}
}
