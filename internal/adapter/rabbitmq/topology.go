package rabbitmq

import (
	"fmt"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

const (
	ExchangeNotifications = "notifications.topic"
	ExchangeRetry         = "notifications.retry.exchange"
	ExchangeDLQ           = "notifications.dlq.exchange"
)

// RetryTTLs holds the per-level TTLs for retry queues. Inject short TTLs in
// tests so retry loops complete in milliseconds rather than minutes.
type RetryTTLs struct {
	Level1 time.Duration // default 30s
	Level2 time.Duration // default 2m
	Level3 time.Duration // default 10m
}

// ProductionRetryTTLs returns the default production TTLs.
func ProductionRetryTTLs() RetryTTLs {
	return RetryTTLs{
		Level1: 30 * time.Second,
		Level2: 120 * time.Second,
		Level3: 600 * time.Second,
	}
}

// QueueBinding pairs a queue name with the routing key it binds to on the
// exchange. For Phase 3 the routing key equals the queue name
// (e.g. "email.high" binds to routing key "email.high").
type QueueBinding struct {
	Queue      string
	RoutingKey string
}

// Topology describes the exchange and the set of queue bindings for a single
// logical deployment. Use ProductionTopology() for real workloads; tests
// inject custom topologies with prefixed names to avoid conflicts.
type Topology struct {
	Exchange string
	Bindings []QueueBinding
	// Prefix is the test-isolation prefix applied to all queue names (not the
	// routing keys, which stay canonical). Empty for production.
	Prefix string
	// RetryTTLs controls the TTL of each retry level's holding queue.
	RetryTTLs RetryTTLs
}

// AllProductionBindings returns the 9 queue bindings for the three channels
// and three priority levels.
func AllProductionBindings() []QueueBinding {
	channels := []string{"email", "sms", "push"}
	priorities := []string{"high", "normal", "low"}
	bindings := make([]QueueBinding, 0, len(channels)*len(priorities))
	for _, ch := range channels {
		for _, p := range priorities {
			name := ch + "." + p
			bindings = append(bindings, QueueBinding{Queue: name, RoutingKey: name})
		}
	}
	return bindings
}

// ProductionTopology returns the canonical Phase 3 topology.
func ProductionTopology() Topology {
	return Topology{
		Exchange:  ExchangeNotifications,
		Bindings:  AllProductionBindings(),
		RetryTTLs: ProductionRetryTTLs(),
	}
}

// RetryExchange returns the retry exchange name for this topology. Tests use the
// same global exchange names as production since exchange names don't conflict
// with queue names.
func (t Topology) RetryExchange() string {
	return ExchangeRetry
}

// DLQExchange returns the DLQ exchange name.
func (t Topology) DLQExchange() string {
	return ExchangeDLQ
}

// RetryQueueName returns the queue name for channel+level, applying the prefix.
func (t Topology) RetryQueueName(channel string, level int) string {
	return fmt.Sprintf("%s%s.retry.%d", t.Prefix, channel, level)
}

// DLQQueueName returns the DLQ queue name for a channel.
func (t Topology) DLQQueueName(channel string) string {
	return fmt.Sprintf("%s%s.dlq", t.Prefix, channel)
}

// DeclareTopologyWith declares the main exchange and each queue+binding
// described by t. Tests call this with a test-scoped Topology; production
// calls it via cmd/api and cmd/worker.
func DeclareTopologyWith(ch *amqp.Channel, t Topology) error {
	// --- main exchange ---
	if err := ch.ExchangeDeclare(
		t.Exchange,
		"topic",
		true,  // durable
		false, // auto-delete
		false, // internal
		false, // no-wait
		nil,
	); err != nil {
		return fmt.Errorf("declare exchange %s: %w", t.Exchange, err)
	}

	for _, b := range t.Bindings {
		if _, err := ch.QueueDeclare(
			b.Queue,
			true,  // durable
			false, // auto-delete
			false, // exclusive
			false, // no-wait
			nil,
		); err != nil {
			return fmt.Errorf("declare queue %s: %w", b.Queue, err)
		}
		if err := ch.QueueBind(
			b.Queue,
			b.RoutingKey,
			t.Exchange,
			false,
			nil,
		); err != nil {
			return fmt.Errorf("bind %s->%s: %w", b.Queue, t.Exchange, err)
		}
	}

	// --- retry exchange ---
	if err := ch.ExchangeDeclare(
		ExchangeRetry,
		"topic",
		true,  // durable
		false, // auto-delete
		false, // internal
		false, // no-wait
		nil,
	); err != nil {
		return fmt.Errorf("declare exchange %s: %w", ExchangeRetry, err)
	}

	// --- DLQ exchange ---
	if err := ch.ExchangeDeclare(
		ExchangeDLQ,
		"topic",
		true,  // durable
		false, // auto-delete
		false, // internal
		false, // no-wait
		nil,
	); err != nil {
		return fmt.Errorf("declare exchange %s: %w", ExchangeDLQ, err)
	}

	channels := []string{"email", "sms", "push"}
	ttls := []time.Duration{t.RetryTTLs.Level1, t.RetryTTLs.Level2, t.RetryTTLs.Level3}

	for _, channel := range channels {
		// retry queues: levels 1-3
		for level := 1; level <= 3; level++ {
			qName := t.RetryQueueName(channel, level)
			ttlMs := int(ttls[level-1].Milliseconds())
			// Re-entry into the main exchange uses the channel.normal routing key.
			reEntryKey := channel + ".normal"

			if _, err := ch.QueueDeclare(
				qName,
				true,  // durable
				false, // auto-delete
				false, // exclusive
				false, // no-wait
				amqp.Table{
					"x-message-ttl":          int32(ttlMs),
					"x-dead-letter-exchange": t.Exchange,
					"x-dead-letter-routing-key": reEntryKey,
				},
			); err != nil {
				return fmt.Errorf("declare retry queue %s: %w", qName, err)
			}
			retryRoutingKey := fmt.Sprintf("%s.retry.%d", channel, level)
			if err := ch.QueueBind(
				qName,
				retryRoutingKey,
				ExchangeRetry,
				false,
				nil,
			); err != nil {
				return fmt.Errorf("bind retry queue %s: %w", qName, err)
			}
		}

		// DLQ queue — end-of-line, nothing consumes (admin replay only)
		dlqName := t.DLQQueueName(channel)
		if _, err := ch.QueueDeclare(
			dlqName,
			true,  // durable
			false, // auto-delete
			false, // exclusive
			false, // no-wait
			nil,
		); err != nil {
			return fmt.Errorf("declare dlq queue %s: %w", dlqName, err)
		}
		if err := ch.QueueBind(
			dlqName,
			channel,
			ExchangeDLQ,
			false,
			nil,
		); err != nil {
			return fmt.Errorf("bind dlq queue %s: %w", dlqName, err)
		}
	}

	return nil
}
