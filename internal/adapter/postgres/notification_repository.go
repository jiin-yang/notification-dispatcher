package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jiin-yang/notification-dispatcher/internal/app"
	"github.com/jiin-yang/notification-dispatcher/internal/domain"
)

// ErrNotFound and ErrNotPending are re-exported from the app layer so that
// callers that only import this package can still use errors.Is without
// pulling in app.
var (
	ErrNotFound   = app.ErrNotFound
	ErrNotPending = app.ErrNotPending
)

type NotificationRepository struct {
	pool *pgxpool.Pool
}

func NewNotificationRepository(pool *pgxpool.Pool) *NotificationRepository {
	return &NotificationRepository{pool: pool}
}

const insertQuery = `
INSERT INTO notifications (id, recipient, channel, content, priority, status, correlation_id, batch_id, idempotency_key, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
`

func (r *NotificationRepository) Insert(ctx context.Context, n domain.Notification) error {
	_, err := r.pool.Exec(ctx, insertQuery,
		n.ID, n.Recipient, n.Channel, n.Content, n.Priority, n.Status, n.CorrelationID,
		n.BatchID, n.IdempotencyKey, n.CreatedAt, n.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert notification: %w", err)
	}
	return nil
}

const getByIDQuery = `
SELECT id, recipient, channel, content, priority, status, correlation_id, batch_id, idempotency_key, created_at, updated_at
FROM notifications
WHERE id = $1
`

func (r *NotificationRepository) GetByID(ctx context.Context, id uuid.UUID) (domain.Notification, error) {
	var n domain.Notification
	err := r.pool.QueryRow(ctx, getByIDQuery, id).Scan(
		&n.ID, &n.Recipient, &n.Channel, &n.Content, &n.Priority, &n.Status, &n.CorrelationID,
		&n.BatchID, &n.IdempotencyKey, &n.CreatedAt, &n.UpdatedAt,
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

// InsertBatch inserts the batch header row and all notifications in a single
// transaction using pgx.CopyFrom for efficient bulk insertion.
func (r *NotificationRepository) InsertBatch(ctx context.Context, batchID uuid.UUID, ns []domain.Notification) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin batch tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	_, err = tx.Exec(ctx,
		`INSERT INTO batches (id, total_count) VALUES ($1, $2)`,
		batchID, len(ns),
	)
	if err != nil {
		return fmt.Errorf("insert batch row: %w", err)
	}

	rows := make([][]any, len(ns))
	for i, n := range ns {
		rows[i] = []any{
			n.ID, n.Recipient, n.Channel, n.Content, n.Priority, n.Status,
			n.CorrelationID, n.BatchID, n.IdempotencyKey, n.CreatedAt, n.UpdatedAt,
		}
	}

	cols := []string{
		"id", "recipient", "channel", "content", "priority", "status",
		"correlation_id", "batch_id", "idempotency_key", "created_at", "updated_at",
	}

	_, err = tx.CopyFrom(
		ctx,
		pgx.Identifier{"notifications"},
		cols,
		pgx.CopyFromRows(rows),
	)
	if err != nil {
		return fmt.Errorf("bulk insert notifications: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit batch tx: %w", err)
	}
	return nil
}

// GetBatchSummary returns per-status counts for a batch computed via GROUP BY.
// Returns ErrNotFound if the batch does not exist.
func (r *NotificationRepository) GetBatchSummary(ctx context.Context, batchID uuid.UUID) (app.BatchSummary, error) {
	var total int
	err := r.pool.QueryRow(ctx,
		`SELECT total_count FROM batches WHERE id = $1`, batchID,
	).Scan(&total)
	if errors.Is(err, pgx.ErrNoRows) {
		return app.BatchSummary{}, ErrNotFound
	}
	if err != nil {
		return app.BatchSummary{}, fmt.Errorf("get batch: %w", err)
	}

	statusRows, err := r.pool.Query(ctx,
		`SELECT status, COUNT(*) FROM notifications WHERE batch_id = $1 GROUP BY status`,
		batchID,
	)
	if err != nil {
		return app.BatchSummary{}, fmt.Errorf("query batch summary: %w", err)
	}
	defer statusRows.Close()

	summary := app.BatchSummary{ID: batchID, Total: total}
	for statusRows.Next() {
		var status domain.Status
		var count int
		if err := statusRows.Scan(&status, &count); err != nil {
			return app.BatchSummary{}, fmt.Errorf("scan batch summary row: %w", err)
		}
		switch status {
		case domain.StatusPending:
			summary.Pending = count
		case domain.StatusDelivered:
			summary.Delivered = count
		case domain.StatusFailed:
			summary.Failed = count
		case domain.StatusCancelled:
			summary.Cancelled = count
		}
	}
	if err := statusRows.Err(); err != nil {
		return app.BatchSummary{}, fmt.Errorf("iterate batch summary: %w", err)
	}

	return summary, nil
}

// List returns a paginated, filtered slice of notifications plus the total count
// matching the filter. Both queries run in a read-only transaction for a
// consistent snapshot.
func (r *NotificationRepository) List(ctx context.Context, filter app.ListFilter, page, limit int) ([]domain.Notification, int, error) {
	where, args := buildListWhere(filter)

	countSQL := `SELECT COUNT(*) FROM notifications` + where
	selectSQL := `
SELECT id, recipient, channel, content, priority, status, correlation_id, batch_id, idempotency_key, created_at, updated_at
FROM notifications` + where + `
ORDER BY created_at DESC, id DESC
LIMIT $` + fmt.Sprintf("%d OFFSET $%d", len(args)+1, len(args)+2)

	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, 0, fmt.Errorf("begin list tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var total int
	if err := tx.QueryRow(ctx, countSQL, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count notifications: %w", err)
	}

	offset := (page - 1) * limit
	pageArgs := make([]any, len(args)+2)
	copy(pageArgs, args)
	pageArgs[len(args)] = limit
	pageArgs[len(args)+1] = offset

	listRows, err := tx.Query(ctx, selectSQL, pageArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("list notifications: %w", err)
	}
	defer listRows.Close()

	var items []domain.Notification
	for listRows.Next() {
		var n domain.Notification
		if err := listRows.Scan(
			&n.ID, &n.Recipient, &n.Channel, &n.Content, &n.Priority, &n.Status, &n.CorrelationID,
			&n.BatchID, &n.IdempotencyKey, &n.CreatedAt, &n.UpdatedAt,
		); err != nil {
			return nil, 0, fmt.Errorf("scan notification row: %w", err)
		}
		items = append(items, n)
	}
	if err := listRows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate notifications: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, 0, fmt.Errorf("commit list tx: %w", err)
	}
	return items, total, nil
}

func buildListWhere(f app.ListFilter) (string, []any) {
	var clauses []string
	var args []any

	if f.Status != "" {
		args = append(args, string(f.Status))
		clauses = append(clauses, fmt.Sprintf("status = $%d", len(args)))
	}
	if f.Channel != "" {
		args = append(args, string(f.Channel))
		clauses = append(clauses, fmt.Sprintf("channel = $%d", len(args)))
	}
	if !f.DateFrom.IsZero() {
		args = append(args, f.DateFrom)
		clauses = append(clauses, fmt.Sprintf("created_at >= $%d", len(args)))
	}
	if !f.DateTo.IsZero() {
		args = append(args, f.DateTo)
		clauses = append(clauses, fmt.Sprintf("created_at <= $%d", len(args)))
	}

	if len(clauses) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

// CancelIfPending atomically transitions a notification from pending to
// cancelled. Returns ErrNotFound if no notification with that ID exists,
// ErrNotPending if it exists but is not in pending status.
func (r *NotificationRepository) CancelIfPending(ctx context.Context, id uuid.UUID) (domain.Notification, error) {
	const q = `
UPDATE notifications
SET status = 'cancelled', updated_at = NOW()
WHERE id = $1 AND status = 'pending'
RETURNING id, recipient, channel, content, priority, status, correlation_id, batch_id, idempotency_key, created_at, updated_at
`
	var n domain.Notification
	err := r.pool.QueryRow(ctx, q, id).Scan(
		&n.ID, &n.Recipient, &n.Channel, &n.Content, &n.Priority, &n.Status, &n.CorrelationID,
		&n.BatchID, &n.IdempotencyKey, &n.CreatedAt, &n.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		var exists bool
		checkErr := r.pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM notifications WHERE id = $1)`, id,
		).Scan(&exists)
		if checkErr != nil {
			return domain.Notification{}, fmt.Errorf("existence check: %w", checkErr)
		}
		if !exists {
			return domain.Notification{}, ErrNotFound
		}
		return domain.Notification{}, ErrNotPending
	}
	if err != nil {
		return domain.Notification{}, fmt.Errorf("cancel notification: %w", err)
	}
	return n, nil
}

// GetByIdempotencyKey looks up a notification by its idempotency key.
// Returns ErrNotFound when no row matches.
func (r *NotificationRepository) GetByIdempotencyKey(ctx context.Context, key string) (domain.Notification, error) {
	const q = `
SELECT id, recipient, channel, content, priority, status, correlation_id, batch_id, idempotency_key, created_at, updated_at
FROM notifications
WHERE idempotency_key = $1
`
	var n domain.Notification
	err := r.pool.QueryRow(ctx, q, key).Scan(
		&n.ID, &n.Recipient, &n.Channel, &n.Content, &n.Priority, &n.Status, &n.CorrelationID,
		&n.BatchID, &n.IdempotencyKey, &n.CreatedAt, &n.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Notification{}, ErrNotFound
	}
	if err != nil {
		return domain.Notification{}, fmt.Errorf("get by idempotency key: %w", err)
	}
	return n, nil
}
