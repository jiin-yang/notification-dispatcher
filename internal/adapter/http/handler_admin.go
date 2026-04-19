package http

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/jiin-yang/notification-dispatcher/internal/domain"
)

const (
	defaultReplayLimit = 10
	maxReplayLimit     = 100
)

// AMQPChannelProvider opens a fresh AMQP channel on demand. Admin handlers
// need a channel for QueueInspect and basic-get; using a separate one avoids
// interfering with the publisher's confirm-mode channel.
type AMQPChannelProvider interface {
	Channel() (*amqp.Channel, error)
}

// AdminRetryPublisher is the minimal publishing surface the admin replay
// handler needs: republish a raw body to the main exchange with a routing key.
type AdminRetryPublisher interface {
	PublishToExchange(ctx context.Context, exchange, routingKey string, headers map[string]any, body []byte) error
}

// AdminHandler serves the /admin/* routes.
type AdminHandler struct {
	channelProvider AMQPChannelProvider
	publisher       AdminRetryPublisher
	mainExchange    string
	// queuePrefix is prepended to channel names when constructing queue names.
	// Empty in production; "test_" in e2e tests.
	queuePrefix string
	logger      *slog.Logger
}

// NewAdminHandler constructs the admin handler.
func NewAdminHandler(
	channelProvider AMQPChannelProvider,
	publisher AdminRetryPublisher,
	mainExchange string,
	logger *slog.Logger,
) *AdminHandler {
	if logger == nil {
		logger = slog.Default()
	}
	return &AdminHandler{
		channelProvider: channelProvider,
		publisher:       publisher,
		mainExchange:    mainExchange,
		logger:          logger,
	}
}

// WithQueuePrefix sets a prefix for queue name construction. Used by the e2e
// harness to target test-scoped queues ("test_email.dlq" etc.).
func (h *AdminHandler) WithQueuePrefix(prefix string) *AdminHandler {
	h.queuePrefix = prefix
	return h
}

func (h *AdminHandler) dlqQueueName(channel string) string {
	return h.queuePrefix + channel + ".dlq"
}

// Register mounts admin routes under /admin.
func (h *AdminHandler) Register(r chi.Router) {
	r.Route("/admin", func(r chi.Router) {
		r.Get("/dlq/{channel}", h.inspectDLQ)
		r.Post("/dlq/{channel}/replay", h.replayDLQ)
	})
}

type dlqInspectResponse struct {
	Channel      string `json:"channel"`
	MessageCount int    `json:"messageCount"`
}

// inspectDLQ handles GET /admin/dlq/{channel}.
func (h *AdminHandler) inspectDLQ(w http.ResponseWriter, r *http.Request) {
	channel := chi.URLParam(r, "channel")
	if !domain.Channel(channel).Valid() {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown channel %q", channel))
		return
	}

	ch, err := h.channelProvider.Channel()
	if err != nil {
		h.logger.Error("admin: open amqp channel", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to open amqp channel")
		return
	}
	defer ch.Close()

	queueName := h.dlqQueueName(channel)
	q, err := ch.QueueInspect(queueName)
	if err != nil {
		h.logger.Error("admin: queue inspect", "queue", queueName, "err", err)
		writeError(w, http.StatusInternalServerError, "failed to inspect DLQ")
		return
	}

	writeJSON(w, http.StatusOK, dlqInspectResponse{
		Channel:      channel,
		MessageCount: q.Messages,
	})
}

type dlqReplayResponse struct {
	Replayed  int `json:"replayed"`
	Remaining int `json:"remaining"`
}

// replayDLQ handles POST /admin/dlq/{channel}/replay?limit=N.
func (h *AdminHandler) replayDLQ(w http.ResponseWriter, r *http.Request) {
	channel := chi.URLParam(r, "channel")
	if !domain.Channel(channel).Valid() {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown channel %q", channel))
		return
	}

	limit := defaultReplayLimit
	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		n, err := strconv.Atoi(limitStr)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		if n > maxReplayLimit {
			n = maxReplayLimit
		}
		limit = n
	}

	ch, err := h.channelProvider.Channel()
	if err != nil {
		h.logger.Error("admin: open amqp channel for replay", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to open amqp channel")
		return
	}
	defer ch.Close()

	queueName := h.dlqQueueName(channel)
	replayed := 0

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	for replayed < limit {
		msg, ok, err := ch.Get(queueName, false) // manual ack
		if err != nil {
			h.logger.Error("admin: dlq get", "queue", queueName, "err", err)
			writeError(w, http.StatusInternalServerError, "failed to consume from DLQ")
			return
		}
		if !ok {
			break // queue empty
		}

		// Determine where to replay: use the original routing key preserved in
		// the header when the message was sent to the DLQ.
		originalRoutingKey, _ := msg.Headers["x-original-routing-key"].(string)
		if originalRoutingKey == "" {
			originalRoutingKey = channel + ".normal"
		}

		// Build replay headers: reset attempt count, preserve correlation ID.
		corrID, _ := msg.Headers["correlation_id"].(string)
		replayHeaders := map[string]any{
			"x-attempt":      int32(0),
			"correlation_id": corrID,
		}

		if pubErr := h.publisher.PublishToExchange(ctx, h.mainExchange, originalRoutingKey, replayHeaders, msg.Body); pubErr != nil {
			h.logger.Error("admin: replay publish failed", "err", pubErr)
			// Nack without requeue so it stays in the DLQ for manual inspection.
			_ = msg.Nack(false, false)
			writeError(w, http.StatusInternalServerError, "failed to republish message")
			return
		}

		_ = msg.Ack(false)
		replayed++
	}

	// Re-inspect to get remaining count.
	q, err := ch.QueueInspect(queueName)
	remaining := 0
	if err == nil {
		remaining = q.Messages
	}

	writeJSON(w, http.StatusOK, dlqReplayResponse{
		Replayed:  replayed,
		Remaining: remaining,
	})
}

