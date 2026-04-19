package postgres

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DeliveryAttemptsRepository persists per-attempt records for the retry and
// DLQ audit trail.
type DeliveryAttemptsRepository struct {
	pool *pgxpool.Pool
}

func NewDeliveryAttemptsRepository(pool *pgxpool.Pool) *DeliveryAttemptsRepository {
	return &DeliveryAttemptsRepository{pool: pool}
}

// RecordAttempt inserts one row into delivery_attempts.
// status: "success" | "failed" | "retrying" | "dlq" | "circuit_open"
// errorReason and providerResponse may be empty.
func (r *DeliveryAttemptsRepository) RecordAttempt(
	ctx context.Context,
	notificationID uuid.UUID,
	attemptNumber int,
	status, errorReason, providerResponse string,
) error {
	const q = `
INSERT INTO delivery_attempts (notification_id, attempt_number, status, error_reason, provider_response)
VALUES ($1, $2, $3, NULLIF($4,''), NULLIF($5,''))
`
	_, err := r.pool.Exec(ctx, q, notificationID, attemptNumber, status, errorReason, providerResponse)
	if err != nil {
		return fmt.Errorf("record delivery attempt: %w", err)
	}
	return nil
}

// CountAttempts returns the number of delivery_attempts rows for a notification.
func (r *DeliveryAttemptsRepository) CountAttempts(ctx context.Context, notificationID uuid.UUID) (int, error) {
	var count int
	err := r.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM delivery_attempts WHERE notification_id = $1`,
		notificationID,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count delivery attempts: %w", err)
	}
	return count, nil
}

// ListAttempts returns all delivery_attempts rows for a notification ordered by
// attempt_number asc.
func (r *DeliveryAttemptsRepository) ListAttempts(ctx context.Context, notificationID uuid.UUID) ([]DeliveryAttemptRow, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT id, notification_id, attempt_number, status, error_reason, provider_response, attempted_at
		 FROM delivery_attempts
		 WHERE notification_id = $1
		 ORDER BY attempt_number ASC, id ASC`,
		notificationID,
	)
	if err != nil {
		return nil, fmt.Errorf("list delivery attempts: %w", err)
	}
	defer rows.Close()

	var result []DeliveryAttemptRow
	for rows.Next() {
		var row DeliveryAttemptRow
		if err := rows.Scan(
			&row.ID, &row.NotificationID, &row.AttemptNumber,
			&row.Status, &row.ErrorReason, &row.ProviderResponse, &row.AttemptedAt,
		); err != nil {
			return nil, fmt.Errorf("scan delivery attempt row: %w", err)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate delivery attempts: %w", err)
	}
	return result, nil
}
