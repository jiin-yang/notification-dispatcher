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

// DeclareTopology declares the Phase 1 topology: one topic exchange and one
// catch-all queue. Later phases split per-channel and add DLX.
func DeclareTopology(ch *amqp.Channel) error {
	if err := ch.ExchangeDeclare(
		ExchangeNotifications,
		"topic",
		true,  // durable
		false, // auto-delete
		false, // internal
		false, // no-wait
		nil,
	); err != nil {
		return fmt.Errorf("declare exchange %s: %w", ExchangeNotifications, err)
	}

	if _, err := ch.QueueDeclare(
		QueueDefault,
		true,  // durable
		false, // auto-delete
		false, // exclusive
		false, // no-wait
		nil,
	); err != nil {
		return fmt.Errorf("declare queue %s: %w", QueueDefault, err)
	}

	if err := ch.QueueBind(
		QueueDefault,
		BindingKeyAll,
		ExchangeNotifications,
		false,
		nil,
	); err != nil {
		return fmt.Errorf("bind %s->%s: %w", QueueDefault, ExchangeNotifications, err)
	}

	return nil
}
