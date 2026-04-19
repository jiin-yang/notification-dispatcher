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

type fakeRepo struct {
	inserted []domain.Notification
	insertErr error
	statuses map[uuid.UUID]domain.Status
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{statuses: map[uuid.UUID]domain.Status{}}
}

func (r *fakeRepo) Insert(_ context.Context, n domain.Notification) error {
	if r.insertErr != nil {
		return r.insertErr
	}
	r.inserted = append(r.inserted, n)
	return nil
}

func (r *fakeRepo) GetByID(_ context.Context, id uuid.UUID) (domain.Notification, error) {
	for _, n := range r.inserted {
		if n.ID == id {
			return n, nil
		}
	}
	return domain.Notification{}, errors.New("not found")
}

func (r *fakeRepo) UpdateStatus(_ context.Context, id uuid.UUID, s domain.Status) error {
	r.statuses[id] = s
	return nil
}

type capturedPublish struct {
	routingKey string
	headers    map[string]any
	body       []byte
}

type fakePublisher struct {
	calls []capturedPublish
	err   error
}

func (p *fakePublisher) Publish(_ context.Context, rk string, h map[string]any, body []byte) error {
	if p.err != nil {
		return p.err
	}
	p.calls = append(p.calls, capturedPublish{routingKey: rk, headers: h, body: body})
	return nil
}

func TestNotificationService_Create_PublishesAndPersists(t *testing.T) {
	repo := newFakeRepo()
	pub := &fakePublisher{}
	svc := app.NewNotificationService(repo, pub)

	cid := uuid.New()
	n, err := svc.Create(context.Background(), app.CreateInput{
		Recipient:     "+905551234567",
		Channel:       domain.ChannelSMS,
		Content:       "hi",
		CorrelationID: cid,
	})
	if err != nil {
		t.Fatalf("Create returned %v", err)
	}

	if n.Priority != domain.PriorityNormal {
		t.Errorf("default priority = %q, want normal", n.Priority)
	}
	if n.Status != domain.StatusPending {
		t.Errorf("status = %q, want pending", n.Status)
	}
	if len(repo.inserted) != 1 {
		t.Fatalf("inserted = %d, want 1", len(repo.inserted))
	}
	if len(pub.calls) != 1 {
		t.Fatalf("publish calls = %d, want 1", len(pub.calls))
	}

	call := pub.calls[0]
	if call.routingKey != "sms.normal" {
		t.Errorf("routing key = %q, want sms.normal", call.routingKey)
	}
	if call.headers["correlation_id"] != cid.String() {
		t.Errorf("header correlation_id = %v, want %v", call.headers["correlation_id"], cid)
	}

	var msg app.NotificationMessage
	if err := json.Unmarshal(call.body, &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if msg.ID != n.ID || msg.Recipient != "+905551234567" {
		t.Errorf("message body mismatch: %+v", msg)
	}
}

func TestNotificationService_Create_InvalidInput(t *testing.T) {
	repo := newFakeRepo()
	pub := &fakePublisher{}
	svc := app.NewNotificationService(repo, pub)

	_, err := svc.Create(context.Background(), app.CreateInput{
		Recipient:     "not-a-phone",
		Channel:       domain.ChannelSMS,
		Content:       "hi",
		CorrelationID: uuid.New(),
	})
	if !errors.Is(err, app.ErrInvalidInput) {
		t.Fatalf("want ErrInvalidInput, got %v", err)
	}
	if len(repo.inserted) != 0 {
		t.Errorf("should not insert on validation failure; got %d", len(repo.inserted))
	}
	if len(pub.calls) != 0 {
		t.Errorf("should not publish on validation failure; got %d", len(pub.calls))
	}
}
