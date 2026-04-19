package rabbitmq

import (
	"context"
	"fmt"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// Publisher wraps a channel in confirm mode and serializes publishes so each
// caller can await its own ack/nack. amqp.Channel is not safe for concurrent
// use from multiple goroutines publishing with confirms.
type Publisher struct {
	ch       *amqp.Channel
	exchange string
	mu       sync.Mutex
}

func NewPublisher(ch *amqp.Channel, exchange string) (*Publisher, error) {
	if err := ch.Confirm(false); err != nil {
		return nil, fmt.Errorf("enable publisher confirms: %w", err)
	}
	return &Publisher{ch: ch, exchange: exchange}, nil
}

// Publish sends a persistent message and waits for broker confirmation.
// Returns an error if the broker nacks or the context expires.
func (p *Publisher) Publish(ctx context.Context, routingKey string, headers map[string]any, body []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	conf, err := p.ch.PublishWithDeferredConfirmWithContext(
		ctx,
		p.exchange,
		routingKey,
		true,  // mandatory
		false, // immediate
		amqp.Publishing{
			ContentType:  "application/json",
			DeliveryMode: amqp.Persistent,
			Timestamp:    time.Now().UTC(),
			Headers:      amqp.Table(headers),
			Body:         body,
		},
	)
	if err != nil {
		return fmt.Errorf("publish: %w", err)
	}

	ack, err := conf.WaitContext(ctx)
	if err != nil {
		return fmt.Errorf("await confirm: %w", err)
	}
	if !ack {
		return fmt.Errorf("broker nacked message")
	}
	return nil
}
