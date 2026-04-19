package http

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/jiin-yang/notification-dispatcher/internal/adapter/rabbitmq"
	"github.com/jiin-yang/notification-dispatcher/internal/domain"
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
	r.Use(CorrelationID)
	r.Use(RequestLogger(log))

	NewHealthHandler(deps.Checkers...).Register(r)
	NewNotificationHandler(deps.Service, log, deps.RateLimiter).Register(r)
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
