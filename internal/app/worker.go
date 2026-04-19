package app

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/jiin-yang/notification-dispatcher/internal/adapter/rabbitmq"
	"github.com/jiin-yang/notification-dispatcher/internal/platform/metrics"
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
	// under its own supervisor goroutine that restarts on failure.
	Consumers      []ConsumerRunner
	DeliverUseCase *DeliverUseCase
	Logger         *slog.Logger
	// Metrics is optional. When non-nil, consumer restart events are counted.
	Metrics *metrics.Metrics
}

// RunWorker starts one supervisor goroutine per consumer. A supervisor restarts
// its consumer on failure as long as ctx is not cancelled, using exponential
// backoff (min 500ms, max 30s). RunWorker blocks until all supervisors exit and
// returns nil — a single failing consumer never cancels the others.
func RunWorker(ctx context.Context, deps WorkerDeps) error {
	if len(deps.Consumers) == 0 {
		return fmt.Errorf("RunWorker: no consumers provided")
	}

	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}

	handler := func(ctx context.Context, d amqp.Delivery) error {
		attempt := headerInt32(d.Headers, "x-attempt")
		originalRoutingKey := d.RoutingKey
		return deps.DeliverUseCase.Handle(
			ctx,
			d.Body,
			headerString(d.Headers, "correlation_id"),
			int(attempt),
			originalRoutingKey,
		)
	}

	var wg sync.WaitGroup
	for _, c := range deps.Consumers {
		c := c // capture loop var
		queueName := queueNameFor(c)
		wg.Add(1)
		go func() {
			defer wg.Done()
			supervisorLoop(ctx, c, handler, queueName, deps.Metrics, log)
		}()
	}
	wg.Wait()
	return nil
}

// supervisorLoop runs consumer.Run in a loop, restarting on failure with
// exponential backoff while ctx is active. Backoff state is never reset so
// rapid crash loops converge to the maximum sleep quickly.
func supervisorLoop(
	ctx context.Context,
	consumer ConsumerRunner,
	handle rabbitmq.Handler,
	queueName string,
	m *metrics.Metrics,
	log *slog.Logger,
) {
	const (
		baseDelay = 500 * time.Millisecond
		maxDelay  = 30 * time.Second
	)
	var attempt int

	for {
		if err := consumer.Run(ctx, handle); err != nil {
			if ctx.Err() != nil {
				// Normal shutdown; stop the loop.
				return
			}

			// Crash during active operation — backoff then restart.
			delay := backoffDelay(attempt, baseDelay, maxDelay)
			log.Error("consumer exited with error; restarting",
				"queue", queueName,
				"err", err,
				"attempt", attempt,
				"backoff", delay,
			)
			if m != nil {
				m.ConsumerRestarts.WithLabelValues(queueName).Inc()
			}
			attempt++

			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
		} else {
			// Run returned nil — treat as clean shutdown.
			return
		}
	}
}

// backoffDelay computes min(base * 2^attempt, max).
func backoffDelay(attempt int, base, max time.Duration) time.Duration {
	shift := attempt
	if shift > 30 { // guard against overflow on int64 shift
		shift = 30
	}
	d := base * (1 << uint(shift))
	if d > max || d <= 0 { // d <= 0 catches overflow
		return max
	}
	return d
}

// queueNameFor extracts a queue name from the consumer for logging. The
// *rabbitmq.Consumer type is not imported here (DIP), so we use a type
// assertion against an optional interface.
func queueNameFor(c ConsumerRunner) string {
	type namer interface{ QueueName() string }
	if n, ok := c.(namer); ok {
		return n.QueueName()
	}
	return "unknown"
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

func headerInt32(h amqp.Table, key string) int32 {
	if h == nil {
		return 0
	}
	switch v := h[key].(type) {
	case int32:
		return v
	case int64:
		return int32(v)
	case int:
		return int32(v)
	case uint32:
		return int32(v)
	case uint64:
		return int32(v)
	case float64:
		return int32(v)
	}
	return 0
}
