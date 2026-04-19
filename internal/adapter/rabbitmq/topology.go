package rabbitmq

import (
	"fmt"

	amqp "github.com/rabbitmq/amqp091-go"
)

const (
	ExchangeNotifications = "notifications.topic"
)

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
		Exchange: ExchangeNotifications,
		Bindings: AllProductionBindings(),
	}
}

// DeclareTopologyWith declares the exchange and each queue+binding described
// by t. Tests call this with a test-scoped Topology; production calls it via
// cmd/api and cmd/worker.
func DeclareTopologyWith(ch *amqp.Channel, t Topology) error {
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

	return nil
}
