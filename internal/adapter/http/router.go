package http

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/jiin-yang/notification-dispatcher/internal/adapter/rabbitmq"
	"github.com/jiin-yang/notification-dispatcher/internal/domain"
	"github.com/jiin-yang/notification-dispatcher/internal/platform/metrics"
)

// PingChecker is an alias for HealthChecker so callers that import only the
// router package have a single, self-contained name for the type.
type PingChecker = HealthChecker

// ChannelRateLimiter is the port the HTTP layer uses to check rate limits.
// internal/platform/ratelimit.ChannelLimiter satisfies this interface.
type ChannelRateLimiter interface {
	AllowChannel(ch domain.Channel, n int) (ok bool, retryAfter time.Duration)
}

// RouterDeps groups everything NewRouter needs. Keeping it a struct (not a
// long parameter list) lets tests inject only what they need.
type RouterDeps struct {
	Logger  *slog.Logger
	Service NotificationService
	// Checkers are passed to the health handler. If nil, health returns ok
	// with no component breakdown (suitable for tests that skip infra checks).
	Checkers []HealthChecker
	// RateLimiter applies per-channel ingress limits. If nil, no limiting is
	// applied (useful in tests that don't need rate-limit behavior).
	RateLimiter ChannelRateLimiter
	// Metrics, if non-nil, wires the HTTP metrics middleware, the /metrics
	// endpoint, and gives the notification handler a sink for enqueue counters.
	Metrics *metrics.Metrics
	// Admin fields — both must be non-nil to mount the /admin routes.
	// AMQPChannelProvider opens fresh AMQP channels for DLQ inspect/replay.
	AMQPChannelProvider AMQPChannelProvider
	// AdminPublisher is the publisher used by DLQ replay to re-inject messages.
	AdminPublisher AdminRetryPublisher
	// AdminQueuePrefix is prepended to queue names in admin handlers. Empty in
	// production; "test_" in e2e tests so admin routes target test-scoped queues.
	AdminQueuePrefix string
	// AdminMainExchange overrides the main exchange used for DLQ replay. Defaults
	// to rabbitmq.ExchangeNotifications when empty.
	AdminMainExchange string
}

// NewRouter builds and returns the chi router with the same middleware and
// routes used in production. cmd/api/main.go calls this; e2e tests call it
// directly so they exercise the exact same wiring.
func NewRouter(deps RouterDeps) chi.Router {
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}

	r := chi.NewRouter()
	r.Use(Recoverer(log))
	r.Use(CorrelationID)
	r.Use(RequestLogger(log))
	if deps.Metrics != nil {
		r.Use(Metrics(deps.Metrics))
	}

	NewHealthHandler(deps.Checkers...).Register(r)
	RegisterMetrics(r, deps.Metrics)
	RegisterDocs(r)
	NewNotificationHandler(deps.Service, log, deps.RateLimiter, deps.Metrics).Register(r)

	if deps.AMQPChannelProvider != nil && deps.AdminPublisher != nil {
		mainExchange := deps.AdminMainExchange
		if mainExchange == "" {
			mainExchange = rabbitmq.ExchangeNotifications
		}
		NewAdminHandler(
			deps.AMQPChannelProvider,
			deps.AdminPublisher,
			mainExchange,
			log,
		).WithQueuePrefix(deps.AdminQueuePrefix).Register(r)
	}

	return r
}

// RMQPingChecker wraps rabbitmq.Connection to satisfy PingChecker.
type RMQPingChecker struct{ Conn *rabbitmq.Connection }

func (c RMQPingChecker) Name() string { return "rabbitmq" }
func (c RMQPingChecker) Ping(_ context.Context) error {
	if c.Conn.IsClosed() {
		return errors.New("connection closed")
	}
	return nil
}
