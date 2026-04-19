package provider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/sony/gobreaker/v2"

	"github.com/jiin-yang/notification-dispatcher/internal/app"
	"github.com/jiin-yang/notification-dispatcher/internal/domain"
)

// CBConfig holds per-deployment circuit breaker settings.
type CBConfig struct {
	// FailureThreshold is the number of consecutive failures before the breaker
	// transitions to open.
	FailureThreshold uint32
	// OpenDuration is how long the breaker stays open before moving to
	// half-open.
	OpenDuration time.Duration
	// HalfOpenMaxCalls is the maximum number of calls allowed in half-open
	// state before the breaker decides to close or re-open.
	HalfOpenMaxCalls uint32
}

// DefaultCBConfig returns sensible production defaults.
func DefaultCBConfig() CBConfig {
	return CBConfig{
		FailureThreshold: 5,
		OpenDuration:     30 * time.Second,
		HalfOpenMaxCalls: 2,
	}
}

// CircuitBreakerRegistry wraps a Registry and guards each channel's provider
// with its own gobreaker.CircuitBreaker. One breaker per channel means that an
// SMS provider outage does not affect the email path.
type CircuitBreakerRegistry struct {
	inner    *Registry
	breakers map[domain.Channel]*gobreaker.CircuitBreaker[struct{}]
	logger   *slog.Logger
}

// NewCircuitBreakerRegistry wraps reg with a circuit breaker per channel.
func NewCircuitBreakerRegistry(reg *Registry, cfg CBConfig, logger *slog.Logger) *CircuitBreakerRegistry {
	if logger == nil {
		logger = slog.Default()
	}
	breakers := make(map[domain.Channel]*gobreaker.CircuitBreaker[struct{}], len(reg.providers))
	for ch := range reg.providers {
		ch := ch // capture
		st := gobreaker.Settings{
			Name:        fmt.Sprintf("provider-%s", ch),
			MaxRequests: cfg.HalfOpenMaxCalls,
			Interval:    0,
			Timeout:     cfg.OpenDuration,
			ReadyToTrip: func(counts gobreaker.Counts) bool {
				return counts.ConsecutiveFailures >= cfg.FailureThreshold
			},
			OnStateChange: func(name string, from, to gobreaker.State) {
				logger.Info("circuit breaker state change",
					"breaker", name,
					"channel", string(ch),
					"from", from.String(),
					"to", to.String(),
				)
			},
		}
		breakers[ch] = gobreaker.NewCircuitBreaker[struct{}](st)
	}
	return &CircuitBreakerRegistry{inner: reg, breakers: breakers, logger: logger}
}

// Send dispatches to the inner registry, guarded by the channel's circuit
// breaker. When the breaker is open it returns app.ErrCircuitOpen so the
// deliver use case can route the message to the retry exchange without calling
// the provider. Business logic is never exposed to gobreaker types directly.
func (r *CircuitBreakerRegistry) Send(ctx context.Context, n domain.Notification) error {
	cb, ok := r.breakers[n.Channel]
	if !ok {
		return r.inner.Send(ctx, n)
	}

	_, cbErr := cb.Execute(func() (struct{}, error) {
		sendErr := r.inner.Send(ctx, n)
		if sendErr != nil {
			if ctx.Err() != nil {
				// Return a non-counting sentinel: context errors must not trip
				// the breaker. We re-wrap after Execute so the outer caller sees
				// the ctx error, not gobreaker.ErrOpenState.
				return struct{}{}, gobreaker.ErrOpenState
			}
			return struct{}{}, sendErr
		}
		return struct{}{}, nil
	})

	if cbErr == nil {
		return nil
	}

	// Distinguish breaker-open from a real provider failure.
	if errors.Is(cbErr, gobreaker.ErrOpenState) || errors.Is(cbErr, gobreaker.ErrTooManyRequests) {
		// If the context was cancelled, propagate that (not a circuit trip).
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w: channel %s", app.ErrCircuitOpen, n.Channel)
	}
	return cbErr
}

// BreakerState returns the current state of the circuit breaker for the given
// channel. Returns gobreaker.StateClosed if no breaker exists.
func (r *CircuitBreakerRegistry) BreakerState(ch domain.Channel) gobreaker.State {
	if cb, ok := r.breakers[ch]; ok {
		return cb.State()
	}
	return gobreaker.StateClosed
}

// Channels returns the set of channels this registry has breakers for.
func (r *CircuitBreakerRegistry) Channels() []domain.Channel {
	out := make([]domain.Channel, 0, len(r.breakers))
	for ch := range r.breakers {
		out = append(out, ch)
	}
	return out
}
