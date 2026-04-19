package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/jiin-yang/notification-dispatcher/internal/domain"
)

// ErrNotFound is returned by the service when an entity does not exist.
// Repository implementations must wrap this sentinel (or return it directly)
// so callers can use errors.Is without importing the postgres adapter.
var ErrNotFound = errors.New("not found")

// ErrNotPending is returned by Cancel when the notification exists but its
// status is not pending.
var ErrNotPending = errors.New("notification is not pending")

// Repository is the port the application layer uses for persistence. All
// Phase 1 and Phase 2 methods are declared here so adapters implement one
// interface and the service depends only on this abstraction.
type Repository interface {
	Insert(ctx context.Context, n domain.Notification) error
	GetByID(ctx context.Context, id uuid.UUID) (domain.Notification, error)
	UpdateStatus(ctx context.Context, id uuid.UUID, status domain.Status) error

	InsertBatch(ctx context.Context, batchID uuid.UUID, ns []domain.Notification) error
	GetBatchSummary(ctx context.Context, batchID uuid.UUID) (BatchSummary, error)
	List(ctx context.Context, filter ListFilter, page, limit int) ([]domain.Notification, int, error)
	CancelIfPending(ctx context.Context, id uuid.UUID) (domain.Notification, error)
	GetByIdempotencyKey(ctx context.Context, key string) (domain.Notification, error)
}

// MessagePublisher abstracts RabbitMQ publishing so the app layer stays
// transport-agnostic. headers uses amqp.Table's underlying type.
type MessagePublisher interface {
	Publish(ctx context.Context, routingKey string, headers map[string]any, body []byte) error
}

type Clock func() time.Time

type NotificationService struct {
	repo      Repository
	publisher MessagePublisher
	logger    *slog.Logger
	now       Clock
}

func NewNotificationService(repo Repository, publisher MessagePublisher) *NotificationService {
	return &NotificationService{
		repo:      repo,
		publisher: publisher,
		logger:    slog.Default(),
		now:       func() time.Time { return time.Now().UTC() },
	}
}

type CreateInput struct {
	Recipient      string
	Channel        domain.Channel
	Content        string
	Priority       domain.Priority
	CorrelationID  uuid.UUID
	BatchID        *uuid.UUID
	IdempotencyKey *string
}

var ErrInvalidInput = errors.New("invalid input")

func (s *NotificationService) Create(ctx context.Context, in CreateInput) (domain.Notification, error) {
	priority := in.Priority
	if priority == "" {
		priority = domain.PriorityNormal
	}

	now := s.now()
	n := domain.Notification{
		ID:             uuid.New(),
		Recipient:      in.Recipient,
		Channel:        in.Channel,
		Content:        in.Content,
		Priority:       priority,
		Status:         domain.StatusPending,
		CorrelationID:  in.CorrelationID,
		BatchID:        in.BatchID,
		IdempotencyKey: in.IdempotencyKey,
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	if err := n.Validate(); err != nil {
		return domain.Notification{}, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}

	if err := s.repo.Insert(ctx, n); err != nil {
		return domain.Notification{}, err
	}

	if err := s.publishOne(ctx, n); err != nil {
		return domain.Notification{}, fmt.Errorf("publish notification: %w", err)
	}

	return n, nil
}

// CreateWithIdempotency creates a notification guarded by an idempotency key.
// If a notification with that key already exists the stored notification is
// returned with replayed=true and no publish is attempted.
func (s *NotificationService) CreateWithIdempotency(ctx context.Context, in CreateInput, idempotencyKey string) (n domain.Notification, replayed bool, err error) {
	existing, lookupErr := s.repo.GetByIdempotencyKey(ctx, idempotencyKey)
	if lookupErr == nil {
		return existing, true, nil
	}
	if !errors.Is(lookupErr, ErrNotFound) {
		return domain.Notification{}, false, fmt.Errorf("idempotency lookup: %w", lookupErr)
	}

	in.IdempotencyKey = &idempotencyKey
	created, err := s.Create(ctx, in)
	if err != nil {
		return domain.Notification{}, false, err
	}
	return created, false, nil
}

// CreateBatch inserts a batch header + all notifications in one transaction,
// then publishes each notification. Publish failures after commit are logged
// and do NOT roll back — the rows remain in pending status for the outbox
// relay (Phase 4) to pick up.
func (s *NotificationService) CreateBatch(ctx context.Context, items []CreateInput) (batchID uuid.UUID, created []domain.Notification, err error) {
	batchID = uuid.New()
	now := s.now()

	ns := make([]domain.Notification, len(items))
	for i, in := range items {
		priority := in.Priority
		if priority == "" {
			priority = domain.PriorityNormal
		}
		cid := in.CorrelationID
		if cid == uuid.Nil {
			cid = uuid.New()
		}
		n := domain.Notification{
			ID:            uuid.New(),
			Recipient:     in.Recipient,
			Channel:       in.Channel,
			Content:       in.Content,
			Priority:      priority,
			Status:        domain.StatusPending,
			CorrelationID: cid,
			BatchID:       &batchID,
			CreatedAt:     now,
			UpdatedAt:     now,
		}
		if err := n.Validate(); err != nil {
			return uuid.Nil, nil, fmt.Errorf("%w: item %d: %v", ErrInvalidInput, i, err)
		}
		ns[i] = n
	}

	if err := s.repo.InsertBatch(ctx, batchID, ns); err != nil {
		return uuid.Nil, nil, fmt.Errorf("insert batch: %w", err)
	}

	for _, n := range ns {
		if pubErr := s.publishOne(ctx, n); pubErr != nil {
			// Publish failure after commit: log and continue. The row stays
			// pending so a future relay can republish (Phase 4).
			s.logger.Warn("batch publish failed; row stays pending",
				"notification_id", n.ID,
				"batch_id", batchID,
				"err", pubErr,
			)
		}
	}

	return batchID, ns, nil
}

func (s *NotificationService) Get(ctx context.Context, id uuid.UUID) (domain.Notification, error) {
	return s.repo.GetByID(ctx, id)
}

func (s *NotificationService) GetBatchSummary(ctx context.Context, batchID uuid.UUID) (BatchSummary, error) {
	return s.repo.GetBatchSummary(ctx, batchID)
}

func (s *NotificationService) List(ctx context.Context, filter ListFilter, page, limit int) ([]domain.Notification, int, error) {
	return s.repo.List(ctx, filter, page, limit)
}

func (s *NotificationService) Cancel(ctx context.Context, id uuid.UUID) (domain.Notification, error) {
	return s.repo.CancelIfPending(ctx, id)
}

func (s *NotificationService) MarkDelivered(ctx context.Context, id uuid.UUID) error {
	return s.repo.UpdateStatus(ctx, id, domain.StatusDelivered)
}

func (s *NotificationService) MarkFailed(ctx context.Context, id uuid.UUID) error {
	return s.repo.UpdateStatus(ctx, id, domain.StatusFailed)
}

func (s *NotificationService) publishOne(ctx context.Context, n domain.Notification) error {
	body, err := json.Marshal(MessageFrom(n))
	if err != nil {
		return fmt.Errorf("marshal message: %w", err)
	}
	headers := map[string]any{
		"notification_id": n.ID.String(),
		"correlation_id":  n.CorrelationID.String(),
	}
	return s.publisher.Publish(ctx, RoutingKey(n.Channel, n.Priority), headers, body)
}
