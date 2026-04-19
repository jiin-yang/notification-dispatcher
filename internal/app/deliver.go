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

type DeliverUseCase struct {
	provider domain.Provider
	updater  StatusUpdater
	logger   *slog.Logger
}

func NewDeliverUseCase(provider domain.Provider, updater StatusUpdater, logger *slog.Logger) *DeliverUseCase {
	if logger == nil {
		logger = slog.Default()
	}
	return &DeliverUseCase{provider: provider, updater: updater, logger: logger}
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
			// Terminal in Phase 1; Phase 4 will route to retry exchange.
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
