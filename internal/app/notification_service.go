package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/jiin-yang/notification-dispatcher/internal/domain"
)

type Repository interface {
	Insert(ctx context.Context, n domain.Notification) error
	GetByID(ctx context.Context, id uuid.UUID) (domain.Notification, error)
	UpdateStatus(ctx context.Context, id uuid.UUID, status domain.Status) error
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
	now       Clock
}

func NewNotificationService(repo Repository, publisher MessagePublisher) *NotificationService {
	return &NotificationService{repo: repo, publisher: publisher, now: func() time.Time { return time.Now().UTC() }}
}

type CreateInput struct {
	Recipient     string
	Channel       domain.Channel
	Content       string
	Priority      domain.Priority
	CorrelationID uuid.UUID
}

var ErrInvalidInput = errors.New("invalid input")

func (s *NotificationService) Create(ctx context.Context, in CreateInput) (domain.Notification, error) {
	priority := in.Priority
	if priority == "" {
		priority = domain.PriorityNormal
	}

	now := s.now()
	n := domain.Notification{
		ID:            uuid.New(),
		Recipient:     in.Recipient,
		Channel:       in.Channel,
		Content:       in.Content,
		Priority:      priority,
		Status:        domain.StatusPending,
		CorrelationID: in.CorrelationID,
		CreatedAt:     now,
		UpdatedAt:     now,
	}

	if err := n.Validate(); err != nil {
		return domain.Notification{}, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}

	if err := s.repo.Insert(ctx, n); err != nil {
		return domain.Notification{}, err
	}

	body, err := json.Marshal(MessageFrom(n))
	if err != nil {
		return domain.Notification{}, fmt.Errorf("marshal message: %w", err)
	}

	headers := map[string]any{
		"notification_id": n.ID.String(),
		"correlation_id":  n.CorrelationID.String(),
	}
	if err := s.publisher.Publish(ctx, RoutingKey(n.Channel, n.Priority), headers, body); err != nil {
		return domain.Notification{}, fmt.Errorf("publish notification: %w", err)
	}

	return n, nil
}

func (s *NotificationService) Get(ctx context.Context, id uuid.UUID) (domain.Notification, error) {
	return s.repo.GetByID(ctx, id)
}

func (s *NotificationService) MarkDelivered(ctx context.Context, id uuid.UUID) error {
	return s.repo.UpdateStatus(ctx, id, domain.StatusDelivered)
}

func (s *NotificationService) MarkFailed(ctx context.Context, id uuid.UUID) error {
	return s.repo.UpdateStatus(ctx, id, domain.StatusFailed)
}
