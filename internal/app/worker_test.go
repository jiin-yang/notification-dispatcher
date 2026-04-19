package app_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	dto "github.com/prometheus/client_model/go"

	"github.com/jiin-yang/notification-dispatcher/internal/adapter/rabbitmq"
	"github.com/jiin-yang/notification-dispatcher/internal/app"
	"github.com/jiin-yang/notification-dispatcher/internal/platform/metrics"
)

// countingConsumer fails the first failN calls to Run, then returns nil.
type countingConsumer struct {
	failN   int
	calls   atomic.Int32
	queueNm string
}

func (c *countingConsumer) QueueName() string { return c.queueNm }

func (c *countingConsumer) Run(_ context.Context, _ rabbitmq.Handler) error {
	n := int(c.calls.Add(1))
	if n <= c.failN {
		return errors.New("simulated consumer failure")
	}
	return nil
}

// blockingConsumer blocks until ctx is cancelled, then returns nil.
type blockingConsumer struct {
	queueNm string
	started chan struct{}
}

func (b *blockingConsumer) QueueName() string { return b.queueNm }

func (b *blockingConsumer) Run(ctx context.Context, _ rabbitmq.Handler) error {
	select {
	case b.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return nil
}

var _ = func(_ context.Context, _ amqp.Delivery) error { return nil } // compile guard

// collectCounterValue reads the float64 value of a prometheus.Counter.
func collectCounterValue(t *testing.T, c interface {
	Write(*dto.Metric) error
}) float64 {
	t.Helper()
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		t.Fatalf("read counter: %v", err)
	}
	if m.Counter == nil {
		return 0
	}
	return m.Counter.GetValue()
}

// TestSupervisor_RestartsOnFailure verifies that a consumer that fails N times
// before succeeding causes exactly N restart counter increments and that
// RunWorker returns nil.
func TestSupervisor_RestartsOnFailure(t *testing.T) {
	const failN = 3
	m := metrics.New("test-supervisor")
	c := &countingConsumer{failN: failN, queueNm: "sms.normal"}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := app.RunWorker(ctx, app.WorkerDeps{
		Consumers:      []app.ConsumerRunner{c},
		DeliverUseCase: nil, // not exercised; consumer returns before calling handler
		Metrics:        m,
	})
	if err != nil {
		t.Fatalf("RunWorker returned error: %v", err)
	}

	if got := c.calls.Load(); got != int32(failN+1) {
		t.Errorf("total Run calls = %d, want %d", got, failN+1)
	}

	restarts := collectCounterValue(t, m.ConsumerRestarts.WithLabelValues("sms.normal"))
	if restarts != float64(failN) {
		t.Errorf("consumer_restarts_total = %v, want %d", restarts, failN)
	}
}

// TestSupervisor_GracefulShutdown verifies that cancelling ctx causes
// supervisors to exit cleanly without restarting consumers.
func TestSupervisor_GracefulShutdown(t *testing.T) {
	started := make(chan struct{}, 1)
	c := &blockingConsumer{queueNm: "email.high", started: started}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- app.RunWorker(ctx, app.WorkerDeps{
			Consumers:      []app.ConsumerRunner{c},
			DeliverUseCase: nil,
		})
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("consumer did not start in time")
	}

	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunWorker returned unexpected error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("RunWorker did not return after ctx cancel")
	}
}

// TestRunWorker_NoConsumers verifies that an empty consumer list is rejected.
func TestRunWorker_NoConsumers(t *testing.T) {
	err := app.RunWorker(context.Background(), app.WorkerDeps{})
	if err == nil {
		t.Fatal("expected error for empty consumers list, got nil")
	}
}

// TestSupervisor_NilMetrics_DoesNotPanic ensures that the supervisor does not
// panic when Metrics is nil.
func TestSupervisor_NilMetrics_DoesNotPanic(t *testing.T) {
	c := &countingConsumer{failN: 1, queueNm: "push.low"}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := app.RunWorker(ctx, app.WorkerDeps{
		Consumers:      []app.ConsumerRunner{c},
		DeliverUseCase: nil,
		Metrics:        nil,
	})
	if err != nil {
		t.Fatalf("RunWorker returned error: %v", err)
	}
}
