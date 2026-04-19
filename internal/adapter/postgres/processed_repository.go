package postgres

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ProcessedRepository persists message dedup records for the consumer
// idempotency guard. Each successfully-dequeued message_id is written exactly
// once; duplicates are silently discarded via ON CONFLICT DO NOTHING.
type ProcessedRepository struct {
	pool *pgxpool.Pool
}

func NewProcessedRepository(pool *pgxpool.Pool) *ProcessedRepository {
	return &ProcessedRepository{pool: pool}
}

// MarkIfUnprocessed attempts to insert message_id into processed_messages.
// Returns (true, nil) when the row was inserted (first time seen).
// Returns (false, nil) when the row already existed (duplicate — ack and skip).
// Returns (false, err) on a real DB error.
func (r *ProcessedRepository) MarkIfUnprocessed(ctx context.Context, messageID uuid.UUID) (bool, error) {
	const q = `
INSERT INTO processed_messages (message_id)
VALUES ($1)
ON CONFLICT (message_id) DO NOTHING
`
	ct, err := r.pool.Exec(ctx, q, messageID)
	if err != nil {
		return false, fmt.Errorf("mark processed: %w", err)
	}
	return ct.RowsAffected() == 1, nil
}

// DeleteProcessed removes the dedup record for messageID so that the message
// can be replayed from the DLQ. A no-op if the row does not exist.
func (r *ProcessedRepository) DeleteProcessed(ctx context.Context, messageID uuid.UUID) error {
	_, err := r.pool.Exec(ctx,
		`DELETE FROM processed_messages WHERE message_id = $1`,
		messageID,
	)
	if err != nil {
		return fmt.Errorf("delete processed: %w", err)
	}
	return nil
}
