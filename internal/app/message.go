package app

import (
	"github.com/google/uuid"

	"github.com/jiin-yang/notification-dispatcher/internal/domain"
)

// NotificationMessage is the wire format exchanged between API and worker via
// RabbitMQ. Keeping this separate from domain.Notification lets the two
// evolve (e.g., extra routing fields) without leaking persistence concerns.
type NotificationMessage struct {
	ID            uuid.UUID       `json:"id"`
	Recipient     string          `json:"recipient"`
	Channel       domain.Channel  `json:"channel"`
	Content       string          `json:"content"`
	Priority      domain.Priority `json:"priority"`
	CorrelationID uuid.UUID       `json:"correlation_id"`
}

func MessageFrom(n domain.Notification) NotificationMessage {
	return NotificationMessage{
		ID:            n.ID,
		Recipient:     n.Recipient,
		Channel:       n.Channel,
		Content:       n.Content,
		Priority:      n.Priority,
		CorrelationID: n.CorrelationID,
	}
}

func (m NotificationMessage) ToNotification() domain.Notification {
	return domain.Notification{
		ID:            m.ID,
		Recipient:     m.Recipient,
		Channel:       m.Channel,
		Content:       m.Content,
		Priority:      m.Priority,
		CorrelationID: m.CorrelationID,
	}
}

// RoutingKey picks the AMQP routing key for a notification. Phase 1 uses a
// single catch-all queue, but producing a channel.priority key now keeps the
// Phase 3 split a no-op at the API layer.
func RoutingKey(channel domain.Channel, priority domain.Priority) string {
	if priority == "" {
		priority = domain.PriorityNormal
	}
	return string(channel) + "." + string(priority)
}
