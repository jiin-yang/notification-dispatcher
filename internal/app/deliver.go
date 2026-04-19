package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/jiin-yang/notification-dispatcher/internal/domain"
)

type StatusUpdater interface {
	UpdateStatus(ctx context.Context, id uuid.UUID, status domain.Status) error
}

// ProcessedMarker records that a message has been handled, and detects
// duplicates. It is separate from StatusUpdater (ISP): the dedup store does
// not need to know about notification status and vice versa.
type ProcessedMarker interface {
	// MarkIfUnprocessed inserts the message_id and returns (true, nil) the
	// first time, (false, nil) if already present, or (false, err) on DB error.
	MarkIfUnprocessed(ctx context.Context, messageID uuid.UUID) (bool, error)
}

type DeliverUseCase struct {
	provider  domain.Provider
	updater   StatusUpdater
	processed ProcessedMarker // may be nil (no dedup, useful in unit tests)
	logger    *slog.Logger
}

func NewDeliverUseCase(provider domain.Provider, updater StatusUpdater, logger *slog.Logger) *DeliverUseCase {
	if logger == nil {
		logger = slog.Default()
	}
	return &DeliverUseCase{provider: provider, updater: updater, logger: logger}
}

// WithProcessedMarker attaches a dedup store. Call before starting the
// consumer loop; not safe for concurrent modification.
func (u *DeliverUseCase) WithProcessedMarker(pm ProcessedMarker) *DeliverUseCase {
	u.processed = pm
	return u
}

// Handle processes a single notification message. It is safe to call
// concurrently; downstream repository and provider are expected to handle
// their own concurrency.
func (u *DeliverUseCase) Handle(ctx context.Context, body []byte, correlationID string) error {
	var msg NotificationMessage
	if err := json.Unmarshal(body, &msg); err != nil {
		return fmt.Errorf("unmarshal message: %w", err)
	}

	log := u.logger.With(
		"correlation_id", correlationID,
		"notification_id", msg.ID.String(),
		"channel", msg.Channel,
	)

	// Idempotency guard: skip if already processed.
	if u.processed != nil {
		inserted, err := u.processed.MarkIfUnprocessed(ctx, msg.ID)
		if err != nil {
			log.Error("idempotency check failed", "err", err)
			return fmt.Errorf("idempotency check: %w", err)
		}
		if !inserted {
			log.Info("duplicate message; skipping")
			return nil // ack without calling provider
		}
	}

	n := msg.ToNotification()
	if err := u.provider.Send(ctx, n); err != nil {
		log.Error("delivery failed", "err", err)
		if ctx.Err() != nil {
			// Shutdown/cancellation: the delivery didn't actually fail
			// terminally — don't mark the row or ack. Returning the error
			// causes the consumer to nack so the broker requeues.
			return err
		}
		if updateErr := u.updater.UpdateStatus(ctx, msg.ID, domain.StatusFailed); updateErr != nil {
			log.Error("mark failed status failed", "err", updateErr)
		}
		if errors.Is(err, domain.ErrDeliveryFailed) {
			// Terminal in Phase 1/3; Phase 4 will route to retry exchange.
			return nil
		}
		return err
	}

	if err := u.updater.UpdateStatus(ctx, msg.ID, domain.StatusDelivered); err != nil {
		log.Error("mark delivered status failed", "err", err)
		return err
	}
	log.Info("delivered")
	return nil
}
