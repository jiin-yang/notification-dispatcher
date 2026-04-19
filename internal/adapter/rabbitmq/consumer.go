package rabbitmq

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// Handler processes a single delivery. Returning nil acks; returning an error
// nacks without requeue.
type Handler func(ctx context.Context, d amqp.Delivery) error

// AMQPChannel is the subset of *amqp.Channel the Consumer needs. Keeping it
// narrow allows tests to substitute a fake without a live broker.
type AMQPChannel interface {
	Qos(prefetchCount, prefetchSize int, global bool) error
	ConsumeWithContext(ctx context.Context, queue, consumer string, autoAck, exclusive, noLocal, noWait bool, args amqp.Table) (<-chan amqp.Delivery, error)
	Cancel(consumer string, noWait bool) error
}

type Consumer struct {
	ch              AMQPChannel
	queue           string
	consumerTag     string
	prefetchCount   int
	shutdownTimeout time.Duration
	sem             chan struct{}
	logger          *slog.Logger
}

type ConsumerOptions struct {
	ConsumerTag     string
	PrefetchCount   int
	ShutdownTimeout time.Duration
	// Concurrency is the maximum number of messages processed in parallel.
	// Defaults to 1. If PrefetchCount < Concurrency, PrefetchCount is raised
	// to match so the broker can fill the semaphore.
	Concurrency int
	Logger      *slog.Logger
}

// NewConsumer creates a Consumer. ch must not be shared across goroutines.
// *amqp.Channel satisfies AMQPChannel.
func NewConsumer(ch AMQPChannel, queue string, opts ConsumerOptions) (*Consumer, error) {
	if opts.Concurrency <= 0 {
		opts.Concurrency = 1
	}
	if opts.PrefetchCount <= 0 {
		opts.PrefetchCount = 10
	}
	// Invariant: PrefetchCount must be >= Concurrency; otherwise the broker
	// never sends enough messages to keep all goroutine slots busy.
	if opts.PrefetchCount < opts.Concurrency {
		if opts.Logger != nil {
			opts.Logger.Warn("PrefetchCount raised to match Concurrency",
				"queue", queue,
				"old_prefetch", opts.PrefetchCount,
				"new_prefetch", opts.Concurrency,
			)
		}
		opts.PrefetchCount = opts.Concurrency
	}
	if opts.ShutdownTimeout <= 0 {
		opts.ShutdownTimeout = 10 * time.Second
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if err := ch.Qos(opts.PrefetchCount, 0, false); err != nil {
		return nil, fmt.Errorf("set qos: %w", err)
	}
	return &Consumer{
		ch:              ch,
		queue:           queue,
		consumerTag:     opts.ConsumerTag,
		prefetchCount:   opts.PrefetchCount,
		shutdownTimeout: opts.ShutdownTimeout,
		sem:             make(chan struct{}, opts.Concurrency),
		logger:          opts.Logger,
	}, nil
}

// QueueName returns the queue this consumer is attached to.
func (c *Consumer) QueueName() string { return c.queue }

// Run blocks until ctx is cancelled or the delivery channel closes. On
// cancellation it stops accepting new deliveries and drains in-flight
// deliveries with a fresh, bounded context so handlers can actually complete;
// anything still unacked when the channel closes is requeued by the broker.
func (c *Consumer) Run(ctx context.Context, handle Handler) error {
	deliveries, err := c.ch.ConsumeWithContext(
		ctx,
		c.queue,
		c.consumerTag,
		false, // autoAck
		false, // exclusive
		false, // noLocal
		false, // noWait
		nil,
	)
	if err != nil {
		return fmt.Errorf("start consume on %s: %w", c.queue, err)
	}

	for {
		select {
		case <-ctx.Done():
			return c.drain(deliveries, handle)
		case d, ok := <-deliveries:
			if !ok {
				return fmt.Errorf("delivery channel closed")
			}
			c.process(ctx, d, handle)
		}
	}
}

func (c *Consumer) drain(deliveries <-chan amqp.Delivery, handle Handler) error {
	if cancelErr := c.ch.Cancel(c.consumerTag, false); cancelErr != nil {
		c.logger.Warn("cancel consumer failed", "err", cancelErr)
	}
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), c.shutdownTimeout)
	defer cancelDrain()

	var wg sync.WaitGroup
	for {
		select {
		case <-drainCtx.Done():
			c.logger.Warn("drain timeout; unacked deliveries will be requeued by broker",
				"timeout", c.shutdownTimeout,
			)
			wg.Wait()
			return nil
		case d, ok := <-deliveries:
			if !ok {
				wg.Wait()
				return nil
			}
			wg.Add(1)
			go func(del amqp.Delivery) {
				defer wg.Done()
				c.processSync(drainCtx, del, handle)
			}(d)
		}
	}
}

// process acquires a semaphore slot and spawns a goroutine to handle the
// delivery. The goroutine owns the ack/nack so the caller never blocks on
// slow handlers.
func (c *Consumer) process(ctx context.Context, d amqp.Delivery, handle Handler) {
	c.sem <- struct{}{}
	go func() {
		defer func() { <-c.sem }()
		c.processSync(ctx, d, handle)
	}()
}

// processSync runs the handler synchronously and acks or nacks the delivery.
func (c *Consumer) processSync(ctx context.Context, d amqp.Delivery, handle Handler) {
	if err := handle(ctx, d); err != nil {
		c.logger.Error("handler failed", "err", err, "routing_key", d.RoutingKey)
		if nackErr := d.Nack(false, false); nackErr != nil {
			c.logger.Error("nack failed", "err", nackErr)
		}
		return
	}
	if ackErr := d.Ack(false); ackErr != nil {
		c.logger.Error("ack failed", "err", ackErr)
	}
}
