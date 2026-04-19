package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jiin-yang/notification-dispatcher/internal/domain"
)

var ErrNotFound = errors.New("notification not found")

type NotificationRepository struct {
	pool *pgxpool.Pool
}

func NewNotificationRepository(pool *pgxpool.Pool) *NotificationRepository {
	return &NotificationRepository{pool: pool}
}

const insertQuery = `
INSERT INTO notifications (id, recipient, channel, content, priority, status, correlation_id, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
`

func (r *NotificationRepository) Insert(ctx context.Context, n domain.Notification) error {
	_, err := r.pool.Exec(ctx, insertQuery,
		n.ID, n.Recipient, n.Channel, n.Content, n.Priority, n.Status, n.CorrelationID, n.CreatedAt, n.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert notification: %w", err)
	}
	return nil
}

const getByIDQuery = `
SELECT id, recipient, channel, content, priority, status, correlation_id, created_at, updated_at
FROM notifications
WHERE id = $1
`

func (r *NotificationRepository) GetByID(ctx context.Context, id uuid.UUID) (domain.Notification, error) {
	var n domain.Notification
	err := r.pool.QueryRow(ctx, getByIDQuery, id).Scan(
		&n.ID, &n.Recipient, &n.Channel, &n.Content, &n.Priority, &n.Status, &n.CorrelationID, &n.CreatedAt, &n.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Notification{}, ErrNotFound
	}
	if err != nil {
		return domain.Notification{}, fmt.Errorf("get notification: %w", err)
	}
	return n, nil
}

const updateStatusQuery = `
UPDATE notifications
SET status = $2, updated_at = NOW()
WHERE id = $1
`

func (r *NotificationRepository) UpdateStatus(ctx context.Context, id uuid.UUID, status domain.Status) error {
	ct, err := r.pool.Exec(ctx, updateStatusQuery, id, status)
	if err != nil {
		return fmt.Errorf("update status: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
