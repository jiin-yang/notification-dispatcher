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
	"github.com/jiin-yang/notification-dispatcher/internal/platform/metrics"
)

// ErrCircuitOpen is the app-layer sentinel for circuit-breaker fast-fail. The
// provider adapter wraps its own gobreaker error into this sentinel so
// deliver.go can distinguish circuit-open from a real delivery failure without
// importing gobreaker. Business logic never imports the circuit-breaker library
// (DIP).
var ErrCircuitOpen = errors.New("circuit breaker open")

// StatusUpdater transitions a notification's status in the DB.
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
	// DeleteProcessed removes the dedup record so that the message can be
	// replayed from the DLQ. A no-op if the row does not exist.
	DeleteProcessed(ctx context.Context, messageID uuid.UUID) error
}

// AttemptRecorder persists a delivery_attempts row for audit and test
// assertions. Kept as an interface (ISP) so the use case doesn't depend on the
// postgres adapter directly.
type AttemptRecorder interface {
	RecordAttempt(ctx context.Context, notificationID uuid.UUID, attemptNumber int, status, errorReason, providerResponse string) error
}

// RetryPublisher routes a message to the retry or DLQ exchange. The consumer
// wires a *rabbitmq.Publisher to satisfy this interface. Keeping it here in the
// app layer (DIP) means deliver.go never imports amqp091.
type RetryPublisher interface {
	// PublishRetry sends the body to the retry exchange at the given priority
	// and level (1-3). priority is "high", "normal", or "low" and is used to
	// route the message back to the correct priority queue after the TTL expires.
	PublishRetry(ctx context.Context, channel string, priority string, level int, attempt int, correlationID string, body []byte) error
	// PublishDLQ sends the body to the DLQ exchange.
	PublishDLQ(ctx context.Context, channel string, attempt int, correlationID string, originalRoutingKey string, body []byte) error
}

const maxAttempts = 3

// DeliverUseCase processes a single notification delivery message end-to-end:
// parse → idempotency check → provider send → success/retry/DLQ routing.
type DeliverUseCase struct {
	provider       domain.Provider
	updater        StatusUpdater
	processed      ProcessedMarker  // may be nil (no dedup)
	retryPublisher RetryPublisher   // may be nil (Phase 1/2/3 compat)
	attempts       AttemptRecorder  // may be nil
	metrics        *metrics.Metrics // may be nil (no metric emission)
	logger         *slog.Logger
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

// WithRetryPublisher attaches the retry/DLQ publisher. Required for Phase 4
// retry behaviour; omitting it falls back to the Phase 1/3 ack-without-retry
// behaviour for terminal failures.
func (u *DeliverUseCase) WithRetryPublisher(rp RetryPublisher) *DeliverUseCase {
	u.retryPublisher = rp
	return u
}

// WithAttemptRecorder attaches the delivery_attempts recorder.
func (u *DeliverUseCase) WithAttemptRecorder(ar AttemptRecorder) *DeliverUseCase {
	u.attempts = ar
	return u
}

// WithMetrics wires a Prometheus metric sink. Optional.
func (u *DeliverUseCase) WithMetrics(m *metrics.Metrics) *DeliverUseCase {
	u.metrics = m
	return u
}

// Handle processes a single notification message. It is safe to call
// concurrently; downstream repository and provider are expected to handle
// their own concurrency.
//
// attempt is the current attempt count from the x-attempt AMQP header (0 =
// first try). originalRoutingKey is the routing key of the message as it
// arrived, used to preserve the re-entry path for DLQ replay.
func (u *DeliverUseCase) Handle(ctx context.Context, body []byte, correlationID string, attempt int, originalRoutingKey string) error {
	var msg NotificationMessage
	if err := json.Unmarshal(body, &msg); err != nil {
		// Poison message: unparseable body.
		// With retry publisher: send straight to DLQ and ack (no infinite loop).
		// Without retry publisher: return error to trigger nack (Phase 1/3 compat).
		u.logger.Error("poison message: cannot unmarshal body",
			"err", err,
			"correlation_id", correlationID,
			"routing_key", originalRoutingKey,
		)
		if u.retryPublisher != nil {
			channel := channelFromRoutingKey(originalRoutingKey)
			if pubErr := u.retryPublisher.PublishDLQ(ctx, channel, attempt, correlationID, originalRoutingKey, body); pubErr != nil {
				u.logger.Error("failed to publish poison message to DLQ", "err", pubErr)
			}
			if u.metrics != nil {
				u.metrics.NotificationsDLQ.WithLabelValues(channel, "poison").Inc()
			}
			return nil // ack — message is unprocessable
		}
		return fmt.Errorf("unmarshal message: %w", err)
	}

	if u.metrics != nil {
		u.metrics.NotificationsInFlight.Inc()
		defer u.metrics.NotificationsInFlight.Dec()
	}

	log := u.logger.With(
		"correlation_id", correlationID,
		"notification_id", msg.ID.String(),
		"channel", msg.Channel,
		"attempt", attempt,
	)

	// Idempotency guard: skip if already processed.
	// Only apply on attempt=0 (initial delivery). Retry attempts intentionally
	// re-process the same notification_id, so the dedup check would always
	// reject them — we skip it for attempt > 0.
	if u.processed != nil && attempt == 0 {
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
	sendStart := time.Now()
	sendErr := u.provider.Send(ctx, n)
	sendDuration := time.Since(sendStart).Seconds()

	if sendErr == nil {
		if u.metrics != nil {
			u.metrics.DeliveryDuration.WithLabelValues(string(msg.Channel), "success").Observe(sendDuration)
			u.metrics.NotificationsDelivered.WithLabelValues(string(msg.Channel), "success").Inc()
		}
		if err := u.updater.UpdateStatus(ctx, msg.ID, domain.StatusDelivered); err != nil {
			log.Error("mark delivered status failed", "err", err)
			return err
		}
		u.recordAttempt(ctx, msg.ID, attempt, "success", "", "")
		log.Info("delivered")
		return nil
	}

	if u.metrics != nil {
		outcome := "failure"
		if errors.Is(sendErr, ErrCircuitOpen) {
			outcome = "circuit_open"
		}
		u.metrics.DeliveryDuration.WithLabelValues(string(msg.Channel), outcome).Observe(sendDuration)
	}

	// Provider returned an error.

	if ctx.Err() != nil {
		// Context cancellation is a caller-side problem — let the consumer nack
		// so the broker requeues the message.
		return sendErr
	}

	isCircuitOpen := errors.Is(sendErr, ErrCircuitOpen)
	isDeliveryFailed := errors.Is(sendErr, domain.ErrDeliveryFailed)

	log.Error("delivery failed",
		"err", sendErr,
		"circuit_open", isCircuitOpen,
		"delivery_failed", isDeliveryFailed,
	)

	// If no retry publisher is wired (Phase 1/2/3 compat), preserve original
	// behaviour: terminal delivery failures ack with status=failed; transient
	// errors nack so the broker requeues.
	if u.retryPublisher == nil {
		if isDeliveryFailed {
			if err := u.updater.UpdateStatus(ctx, msg.ID, domain.StatusFailed); err != nil {
				log.Error("mark failed status failed", "err", err)
			}
			if u.metrics != nil {
				u.metrics.NotificationsDelivered.WithLabelValues(string(msg.Channel), "failed").Inc()
			}
			return nil // ack
		}
		return sendErr // nack — transient error
	}

	// Phase 4: route to retry or DLQ.
	retryLevel := attempt + 1 // level == next attempt number (1-indexed, max 3)
	priorityStr := string(msg.Priority)

	if retryLevel <= maxAttempts {
		// Route to retry exchange.
		status := "retrying"
		errReason := sendErr.Error()
		provResp := ""
		if isCircuitOpen {
			status = "circuit_open"
			errReason = "circuit_breaker_open"
		} else if isDeliveryFailed {
			provResp = sendErr.Error()
		}

		if pubErr := u.retryPublisher.PublishRetry(ctx, string(msg.Channel), priorityStr, retryLevel, retryLevel, correlationID, body); pubErr != nil {
			log.Error("failed to publish to retry exchange", "err", pubErr)
			return fmt.Errorf("retry publish: %w", pubErr)
		}
		u.recordAttempt(ctx, msg.ID, attempt, status, errReason, provResp)
		if u.metrics != nil {
			u.metrics.NotificationsRetried.WithLabelValues(string(msg.Channel), fmt.Sprintf("%d", retryLevel)).Inc()
		}
		log.Info("message routed to retry exchange", "next_attempt", retryLevel)
		// Return nil so the consumer acks the original delivery.
		return nil
	}

	// Exhausted retries → DLQ.
	routingKey := originalRoutingKey
	if routingKey == "" {
		// Preserve priority in the fallback key so admin replay re-enters the
		// correct priority queue instead of always defaulting to normal.
		routingKey = string(msg.Channel) + "." + priorityStr
	}
	if pubErr := u.retryPublisher.PublishDLQ(ctx, string(msg.Channel), attempt, correlationID, routingKey, body); pubErr != nil {
		log.Error("failed to publish to DLQ", "err", pubErr)
		return fmt.Errorf("dlq publish: %w", pubErr)
	}
	if err := u.updater.UpdateStatus(ctx, msg.ID, domain.StatusFailed); err != nil {
		log.Error("mark failed status failed", "err", err)
	}
	// Delete the dedup record so that admin DLQ replay can reprocess this
	// notification. A failure here is non-fatal (replay will simply be
	// skipped by the dedup guard, requiring manual dedup cleanup).
	if u.processed != nil {
		if delErr := u.processed.DeleteProcessed(ctx, msg.ID); delErr != nil {
			log.Error("delete processed record failed (replay will be blocked)", "err", delErr)
		}
	}
	u.recordAttempt(ctx, msg.ID, attempt, "dlq", sendErr.Error(), "")
	if u.metrics != nil {
		reason := "failed"
		if isCircuitOpen {
			reason = "circuit_open"
		}
		u.metrics.NotificationsDLQ.WithLabelValues(string(msg.Channel), reason).Inc()
		u.metrics.NotificationsDelivered.WithLabelValues(string(msg.Channel), "failed").Inc()
	}
	log.Info("message routed to DLQ", "attempt", attempt)
	return nil // ack
}

func (u *DeliverUseCase) recordAttempt(ctx context.Context, id uuid.UUID, attempt int, status, errReason, provResp string) {
	if u.attempts == nil {
		return
	}
	if err := u.attempts.RecordAttempt(ctx, id, attempt, status, errReason, provResp); err != nil {
		u.logger.Error("record attempt failed", "err", err)
	}
}

// channelFromRoutingKey extracts the channel prefix from a routing key like
// "email.normal" or "sms.high". Returns "unknown" if it cannot parse.
func channelFromRoutingKey(key string) string {
	for _, ch := range []string{"email", "sms", "push"} {
		if len(key) >= len(ch) && key[:len(ch)] == ch {
			return ch
		}
	}
	return "unknown"
}
