package app

import (
	"context"
	"log/slog"

	amqp "github.com/rabbitmq/amqp091-go"

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
	Consumer       ConsumerRunner
	DeliverUseCase *DeliverUseCase
	Logger         *slog.Logger
}

// RunWorker starts the consumer loop and blocks until ctx is cancelled or the
// delivery channel closes. It is called from cmd/worker/main.go and from the
// e2e test suite (in a goroutine).
func RunWorker(ctx context.Context, deps WorkerDeps) error {
	return deps.Consumer.Run(ctx, func(ctx context.Context, d amqp.Delivery) error {
		return deps.DeliverUseCase.Handle(ctx, d.Body, headerString(d.Headers, "correlation_id"))
	})
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
