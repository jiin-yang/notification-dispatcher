package http

import (
	"time"

	"github.com/google/uuid"

	"github.com/jiin-yang/notification-dispatcher/internal/domain"
)

type createNotificationRequest struct {
	To       string          `json:"to"`
	Channel  domain.Channel  `json:"channel"`
	Content  string          `json:"content"`
	Priority domain.Priority `json:"priority,omitempty"`
}

type createNotificationResponse struct {
	MessageID uuid.UUID `json:"messageId"`
	Status    string    `json:"status"`
	Timestamp string    `json:"timestamp"`
}

type notificationResponse struct {
	MessageID     uuid.UUID       `json:"messageId"`
	Recipient     string          `json:"recipient"`
	Channel       domain.Channel  `json:"channel"`
	Content       string          `json:"content"`
	Priority      domain.Priority `json:"priority"`
	Status        domain.Status   `json:"status"`
	CorrelationID uuid.UUID       `json:"correlationId"`
	CreatedAt     time.Time       `json:"createdAt"`
	UpdatedAt     time.Time       `json:"updatedAt"`
}

func notificationToResponse(n domain.Notification) notificationResponse {
	return notificationResponse{
		MessageID:     n.ID,
		Recipient:     n.Recipient,
		Channel:       n.Channel,
		Content:       n.Content,
		Priority:      n.Priority,
		Status:        n.Status,
		CorrelationID: n.CorrelationID,
		CreatedAt:     n.CreatedAt,
		UpdatedAt:     n.UpdatedAt,
	}
}

type errorResponse struct {
	Error string `json:"error"`
}
