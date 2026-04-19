package app

import (
	"context"
	"fmt"
	"log/slog"

	amqp "github.com/rabbitmq/amqp091-go"
	"golang.org/x/sync/errgroup"

	"github.com/jiin-yang/notification-dispatcher/internal/adapter/rabbitmq"
)

// ConsumerRunner is the minimal interface the worker needs from a RabbitMQ
// consumer. *rabbitmq.Consumer satisfies it.
type ConsumerRunner interface {
	Run(ctx context.Context, handle rabbitmq.Handler) error
}

// WorkerDeps groups everything RunWorker needs. Using a struct keeps the
// signature stable as new deps are added in later phases.
type WorkerDeps struct {
	// Consumers is the list of queue consumers to run concurrently. Each runs
	// in its own goroutine. The first consumer to return a non-nil error
	// cancels the others.
	Consumers      []ConsumerRunner
	DeliverUseCase *DeliverUseCase
	Logger         *slog.Logger
}

// RunWorker starts one goroutine per consumer and blocks until all goroutines
// return or the first error cancels the group. Called from cmd/worker/main.go
// and from the e2e test harness.
func RunWorker(ctx context.Context, deps WorkerDeps) error {
	if len(deps.Consumers) == 0 {
		return fmt.Errorf("RunWorker: no consumers provided")
	}

	g, gctx := errgroup.WithContext(ctx)

	handler := func(ctx context.Context, d amqp.Delivery) error {
		return deps.DeliverUseCase.Handle(ctx, d.Body, headerString(d.Headers, "correlation_id"))
	}

	for _, c := range deps.Consumers {
		c := c // capture loop var
		g.Go(func() error {
			return c.Run(gctx, handler)
		})
	}

	return g.Wait()
}

func headerString(h amqp.Table, key string) string {
	if h == nil {
		return ""
	}
	if v, ok := h[key].(string); ok {
		return v
	}
	return ""
}
