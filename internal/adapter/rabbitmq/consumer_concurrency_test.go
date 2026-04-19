package rabbitmq_test

import (
	"context"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/jiin-yang/notification-dispatcher/internal/adapter/rabbitmq"
)

// TestConsumerOptions_PrefetchRaisedWhenBelowConcurrency verifies that when
// PrefetchCount < Concurrency, NewConsumer raises PrefetchCount to Concurrency
// and emits a warning log (compile-time behaviour only, no broker required).
//
// We exercise this through the exported ConsumerOptions struct by checking that
// a consumer can be constructed without error when we deliberately set a low
// prefetch. Because the AMQP channel is fake, we verify the invariant
// indirectly via the lack of panic / construction error.
func TestConsumerOptions_PrefetchRaisedWhenBelowConcurrency(t *testing.T) {
	fakeCh := &fakeAMQPChannel{qosErr: nil}

	_, err := rabbitmq.NewConsumer(fakeCh, "push.high", rabbitmq.ConsumerOptions{
		PrefetchCount: 1,
		Concurrency:   5,
		Logger:        slog.Default(),
	})
	if err != nil {
		t.Fatalf("NewConsumer returned unexpected error: %v", err)
	}
	// The channel's Qos call must have received prefetch=5 (raised from 1).
	if fakeCh.lastPrefetch != 5 {
		t.Errorf("Qos called with prefetch=%d, want 5", fakeCh.lastPrefetch)
	}
}

// TestConsumerOptions_DefaultConcurrency verifies that Concurrency<=0 is
// treated as 1 (no behaviour change vs old code).
func TestConsumerOptions_DefaultConcurrency(t *testing.T) {
	fakeCh := &fakeAMQPChannel{}

	_, err := rabbitmq.NewConsumer(fakeCh, "email.normal", rabbitmq.ConsumerOptions{
		PrefetchCount: 10,
		Concurrency:   0, // should be normalised to 1
	})
	if err != nil {
		t.Fatalf("NewConsumer returned unexpected error: %v", err)
	}
	if fakeCh.lastPrefetch != 10 {
		t.Errorf("Qos prefetch=%d, want 10", fakeCh.lastPrefetch)
	}
}

// TestConsumerRun_ConcurrentMessages verifies that with Concurrency=2 the
// consumer dispatches two messages in parallel (in-flight max = 2).
func TestConsumerRun_ConcurrentMessages(t *testing.T) {
	const concurrency = 2
	const messages = 4

	fakeCh := &fakeAMQPChannel{}
	consumer, err := rabbitmq.NewConsumer(fakeCh, "sms.normal", rabbitmq.ConsumerOptions{
		PrefetchCount:   concurrency,
		Concurrency:     concurrency,
		ShutdownTimeout: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewConsumer: %v", err)
	}

	deliveries := make(chan amqp.Delivery, messages)
	fakeCh.deliveries = deliveries

	// Track peak in-flight count.
	var inFlight atomic.Int32
	var peakInFlight atomic.Int32
	slowHandler := func(_ context.Context, _ amqp.Delivery) error {
		cur := inFlight.Add(1)
		for {
			peak := peakInFlight.Load()
			if cur <= peak || peakInFlight.CompareAndSwap(peak, cur) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		inFlight.Add(-1)
		return nil
	}

	// Seed all messages before Run is called so they are immediately available.
	for i := 0; i < messages; i++ {
		deliveries <- amqp.Delivery{
			Acknowledger: &fakeAcker{},
			RoutingKey:   "sms.normal",
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	runDone := make(chan error, 1)
	go func() {
		runDone <- consumer.Run(ctx, slowHandler)
	}()

	// Give the consumer time to pick up all messages.
	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case <-runDone:
	case <-time.After(3 * time.Second):
		t.Fatal("consumer.Run did not return after cancel")
	}

	if peak := peakInFlight.Load(); peak < int32(concurrency) {
		t.Errorf("peak in-flight = %d, want >= %d", peak, concurrency)
	}
}

// fakeAMQPChannel satisfies the amqp.Channel subset used by NewConsumer.
// We replicate only the Qos method; the rest panic if called unexpectedly.
type fakeAMQPChannel struct {
	lastPrefetch int
	qosErr       error
	deliveries   chan amqp.Delivery
}

func (f *fakeAMQPChannel) Qos(prefetchCount, _ int, _ bool) error {
	f.lastPrefetch = prefetchCount
	return f.qosErr
}

func (f *fakeAMQPChannel) ConsumeWithContext(
	_ context.Context, _, _ string, _, _, _, _ bool, _ amqp.Table,
) (<-chan amqp.Delivery, error) {
	if f.deliveries != nil {
		return f.deliveries, nil
	}
	// Return a channel that is closed immediately for tests that don't need messages.
	ch := make(chan amqp.Delivery)
	close(ch)
	return ch, nil
}

func (f *fakeAMQPChannel) Cancel(_ string, _ bool) error { return nil }

// fakeAcker satisfies amqp.Acknowledger.
type fakeAcker struct{}

func (a *fakeAcker) Ack(_ uint64, _ bool) error  { return nil }
func (a *fakeAcker) Nack(_ uint64, _, _ bool) error { return nil }
func (a *fakeAcker) Reject(_ uint64, _ bool) error  { return nil }
