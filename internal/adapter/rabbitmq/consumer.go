package rabbitmq

import (
	"context"
	"fmt"
	"log/slog"

	amqp "github.com/rabbitmq/amqp091-go"
)

// Handler processes a single delivery. Returning nil acks; returning an error
// nacks without requeue. Phase 4 will introduce retry-exchange routing.
type Handler func(ctx context.Context, d amqp.Delivery) error

type Consumer struct {
	ch            *amqp.Channel
	queue         string
	consumerTag   string
	prefetchCount int
	logger        *slog.Logger
}

type ConsumerOptions struct {
	ConsumerTag   string
	PrefetchCount int
	Logger        *slog.Logger
}

func NewConsumer(ch *amqp.Channel, queue string, opts ConsumerOptions) (*Consumer, error) {
	if opts.PrefetchCount <= 0 {
		opts.PrefetchCount = 10
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if err := ch.Qos(opts.PrefetchCount, 0, false); err != nil {
		return nil, fmt.Errorf("set qos: %w", err)
	}
	return &Consumer{
		ch:            ch,
		queue:         queue,
		consumerTag:   opts.ConsumerTag,
		prefetchCount: opts.PrefetchCount,
		logger:        opts.Logger,
	}, nil
}

// Run blocks until ctx is cancelled or the delivery channel closes. It cancels
// the consumer tag on shutdown so in-flight messages finish before the channel
// closes.
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
			if cancelErr := c.ch.Cancel(c.consumerTag, false); cancelErr != nil {
				c.logger.Warn("cancel consumer failed", "err", cancelErr)
			}
			for d := range deliveries {
				c.process(ctx, d, handle)
			}
			return nil
		case d, ok := <-deliveries:
			if !ok {
				return fmt.Errorf("delivery channel closed")
			}
			c.process(ctx, d, handle)
		}
	}
}

func (c *Consumer) process(ctx context.Context, d amqp.Delivery, handle Handler) {
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
