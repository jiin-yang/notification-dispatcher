package rabbitmq

import (
	"fmt"

	amqp "github.com/rabbitmq/amqp091-go"
)

const (
	ExchangeNotifications = "notifications.topic"
	QueueDefault          = "notifications.default"
	BindingKeyAll         = "#"
)

// Topology holds the exchange and queue names for a single logical topology.
// Use DefaultTopology() for production names; tests inject custom names to
// avoid conflicting with a concurrently-running dev environment.
type Topology struct {
	Exchange   string
	Queue      string
	BindingKey string
}

// DefaultTopology returns the production topology names.
func DefaultTopology() Topology {
	return Topology{
		Exchange:   ExchangeNotifications,
		Queue:      QueueDefault,
		BindingKey: BindingKeyAll,
	}
}

// DeclareTopology declares the Phase 1 topology using production defaults.
// Kept for backward compatibility with cmd/api and cmd/worker.
func DeclareTopology(ch *amqp.Channel) error {
	return DeclareTopologyWith(ch, DefaultTopology())
}

// DeclareTopologyWith declares the topology using the provided names. Tests
// call this directly with a test-scoped Topology.
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

	if _, err := ch.QueueDeclare(
		t.Queue,
		true,  // durable
		false, // auto-delete
		false, // exclusive
		false, // no-wait
		nil,
	); err != nil {
		return fmt.Errorf("declare queue %s: %w", t.Queue, err)
	}

	if err := ch.QueueBind(
		t.Queue,
		t.BindingKey,
		t.Exchange,
		false,
		nil,
	); err != nil {
		return fmt.Errorf("bind %s->%s: %w", t.Queue, t.Exchange, err)
	}

	return nil
}
