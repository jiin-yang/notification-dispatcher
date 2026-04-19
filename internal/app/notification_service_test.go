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

// fakeRepo implements app.Repository for unit tests. Only the methods exercised
// by the service under test need real implementations; the rest return zero values.
type fakeRepo struct {
	inserted  []domain.Notification
	insertErr error
	statuses  map[uuid.UUID]domain.Status
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
	return domain.Notification{}, app.ErrNotFound
}

func (r *fakeRepo) UpdateStatus(_ context.Context, id uuid.UUID, s domain.Status) error {
	r.statuses[id] = s
	return nil
}

func (r *fakeRepo) InsertBatch(_ context.Context, _ uuid.UUID, ns []domain.Notification) error {
	r.inserted = append(r.inserted, ns...)
	return nil
}

func (r *fakeRepo) GetBatchSummary(_ context.Context, batchID uuid.UUID) (app.BatchSummary, error) {
	summary := app.BatchSummary{ID: batchID}
	for _, n := range r.inserted {
		if n.BatchID != nil && *n.BatchID == batchID {
			summary.Total++
			switch n.Status {
			case domain.StatusPending:
				summary.Pending++
			case domain.StatusDelivered:
				summary.Delivered++
			case domain.StatusFailed:
				summary.Failed++
			case domain.StatusCancelled:
				summary.Cancelled++
			}
		}
	}
	return summary, nil
}

func (r *fakeRepo) List(_ context.Context, filter app.ListFilter, page, limit int) ([]domain.Notification, int, error) {
	var matched []domain.Notification
	for _, n := range r.inserted {
		if filter.Status != "" && n.Status != filter.Status {
			continue
		}
		if filter.Channel != "" && n.Channel != filter.Channel {
			continue
		}
		matched = append(matched, n)
	}
	total := len(matched)
	start := (page - 1) * limit
	if start >= total {
		return nil, total, nil
	}
	end := start + limit
	if end > total {
		end = total
	}
	return matched[start:end], total, nil
}

func (r *fakeRepo) CancelIfPending(_ context.Context, id uuid.UUID) (domain.Notification, error) {
	for i, n := range r.inserted {
		if n.ID == id {
			if n.Status != domain.StatusPending {
				return domain.Notification{}, app.ErrNotPending
			}
			r.inserted[i].Status = domain.StatusCancelled
			return r.inserted[i], nil
		}
	}
	return domain.Notification{}, app.ErrNotFound
}

func (r *fakeRepo) GetByIdempotencyKey(_ context.Context, key string) (domain.Notification, error) {
	for _, n := range r.inserted {
		if n.IdempotencyKey != nil && *n.IdempotencyKey == key {
			return n, nil
		}
	}
	return domain.Notification{}, app.ErrNotFound
}

// ---- publisher stub ---------------------------------------------------------

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

// ---- tests ------------------------------------------------------------------

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

func TestNotificationService_CreateBatch_PublishesAll(t *testing.T) {
	repo := newFakeRepo()
	pub := &fakePublisher{}
	svc := app.NewNotificationService(repo, pub)

	items := []app.CreateInput{
		{Recipient: "+905551234567", Channel: domain.ChannelSMS, Content: "a", CorrelationID: uuid.New()},
		{Recipient: "user@example.com", Channel: domain.ChannelEmail, Content: "b", CorrelationID: uuid.New()},
	}

	batchID, created, err := svc.CreateBatch(context.Background(), items)
	if err != nil {
		t.Fatalf("CreateBatch returned %v", err)
	}
	if batchID == uuid.Nil {
		t.Error("batchID should not be nil uuid")
	}
	if len(created) != 2 {
		t.Errorf("created = %d, want 2", len(created))
	}
	if len(pub.calls) != 2 {
		t.Errorf("publish calls = %d, want 2", len(pub.calls))
	}
	for _, n := range created {
		if n.BatchID == nil || *n.BatchID != batchID {
			t.Errorf("notification batch_id not set correctly, got %v", n.BatchID)
		}
	}
}

func TestNotificationService_CreateBatch_ValidationFailure(t *testing.T) {
	repo := newFakeRepo()
	pub := &fakePublisher{}
	svc := app.NewNotificationService(repo, pub)

	items := []app.CreateInput{
		{Recipient: "+905551234567", Channel: domain.ChannelSMS, Content: "a", CorrelationID: uuid.New()},
		{Recipient: "bad-phone", Channel: domain.ChannelSMS, Content: "b", CorrelationID: uuid.New()},
	}

	_, _, err := svc.CreateBatch(context.Background(), items)
	if !errors.Is(err, app.ErrInvalidInput) {
		t.Fatalf("want ErrInvalidInput, got %v", err)
	}
	if len(repo.inserted) != 0 {
		t.Errorf("should not insert on validation failure, got %d", len(repo.inserted))
	}
}

func TestNotificationService_Cancel_Pending(t *testing.T) {
	repo := newFakeRepo()
	pub := &fakePublisher{}
	svc := app.NewNotificationService(repo, pub)

	n, _ := svc.Create(context.Background(), app.CreateInput{
		Recipient:     "+905551234567",
		Channel:       domain.ChannelSMS,
		Content:       "hi",
		CorrelationID: uuid.New(),
	})

	cancelled, err := svc.Cancel(context.Background(), n.ID)
	if err != nil {
		t.Fatalf("Cancel returned %v", err)
	}
	if cancelled.Status != domain.StatusCancelled {
		t.Errorf("status = %q, want cancelled", cancelled.Status)
	}
}

func TestNotificationService_Cancel_NotFound(t *testing.T) {
	repo := newFakeRepo()
	pub := &fakePublisher{}
	svc := app.NewNotificationService(repo, pub)

	_, err := svc.Cancel(context.Background(), uuid.New())
	if !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestNotificationService_Idempotency_Replay(t *testing.T) {
	repo := newFakeRepo()
	pub := &fakePublisher{}
	svc := app.NewNotificationService(repo, pub)

	in := app.CreateInput{
		Recipient:     "+905551234567",
		Channel:       domain.ChannelSMS,
		Content:       "hello",
		CorrelationID: uuid.New(),
	}

	n1, replayed1, err := svc.CreateWithIdempotency(context.Background(), in, "key-abc")
	if err != nil {
		t.Fatalf("first CreateWithIdempotency: %v", err)
	}
	if replayed1 {
		t.Error("first call should not be a replay")
	}

	n2, replayed2, err := svc.CreateWithIdempotency(context.Background(), in, "key-abc")
	if err != nil {
		t.Fatalf("second CreateWithIdempotency: %v", err)
	}
	if !replayed2 {
		t.Error("second call should be a replay")
	}
	if n1.ID != n2.ID {
		t.Errorf("ids differ: %v vs %v", n1.ID, n2.ID)
	}
	if len(repo.inserted) != 1 {
		t.Errorf("should have exactly 1 row, got %d", len(repo.inserted))
	}
	if len(pub.calls) != 1 {
		t.Errorf("should have exactly 1 publish call, got %d", len(pub.calls))
	}
}
