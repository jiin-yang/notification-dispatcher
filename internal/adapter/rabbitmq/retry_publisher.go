package rabbitmq

import (
	"context"
	"fmt"
)

// RetryPublisherAdapter wraps a *Publisher and satisfies app.RetryPublisher.
// It knows the retry and DLQ exchange names from the Topology so the app layer
// never needs to know them.
type RetryPublisherAdapter struct {
	pub  *Publisher
	topo Topology
}

// NewRetryPublisherAdapter constructs the adapter.
func NewRetryPublisherAdapter(pub *Publisher, topo Topology) *RetryPublisherAdapter {
	return &RetryPublisherAdapter{pub: pub, topo: topo}
}

// PublishRetry routes body to the retry exchange at the given priority and
// level with an updated x-attempt header. level is 1-3. priority is one of
// "high", "normal", "low" and is embedded in the routing key so the message
// re-enters the correct priority queue after the TTL expires.
func (a *RetryPublisherAdapter) PublishRetry(
	ctx context.Context,
	channel string,
	priority string,
	level int,
	attempt int,
	correlationID string,
	body []byte,
) error {
	routingKey := fmt.Sprintf("%s.%s.retry.%d", channel, priority, level)
	headers := map[string]any{
		"x-attempt":          int32(attempt),
		"correlation_id":     correlationID,
		"x-original-channel": channel,
	}
	if err := a.pub.PublishToExchange(ctx, ExchangeRetry, routingKey, headers, body); err != nil {
		return fmt.Errorf("publish retry level %d for channel %s priority %s: %w", level, channel, priority, err)
	}
	return nil
}

// PublishDLQ routes body to the DLQ exchange. originalRoutingKey is stored in a
// header so admin replay can restore the original routing.
func (a *RetryPublisherAdapter) PublishDLQ(
	ctx context.Context,
	channel string,
	attempt int,
	correlationID string,
	originalRoutingKey string,
	body []byte,
) error {
	headers := map[string]any{
		"x-attempt":              int32(attempt),
		"correlation_id":         correlationID,
		"x-original-routing-key": originalRoutingKey,
		"x-original-channel":     channel,
	}
	if err := a.pub.PublishToExchange(ctx, ExchangeDLQ, channel, headers, body); err != nil {
		return fmt.Errorf("publish dlq for channel %s: %w", channel, err)
	}
	return nil
}
