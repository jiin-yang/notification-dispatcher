package http

import (
	"time"

	"github.com/google/uuid"

	"github.com/jiin-yang/notification-dispatcher/internal/app"
	"github.com/jiin-yang/notification-dispatcher/internal/domain"
)

// ---- single create ----------------------------------------------------------

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

// ---- notification detail ----------------------------------------------------

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

// ---- batch create -----------------------------------------------------------

type batchItemRequest struct {
	To       string          `json:"to"`
	Channel  domain.Channel  `json:"channel"`
	Content  string          `json:"content"`
	Priority domain.Priority `json:"priority,omitempty"`
}

type batchCreateRequest struct {
	Notifications []batchItemRequest `json:"notifications"`
}

type batchCreateResponse struct {
	BatchID   uuid.UUID `json:"batchId"`
	Accepted  int       `json:"accepted"`
	Timestamp string    `json:"timestamp"`
}

// ---- batch summary ----------------------------------------------------------

type batchSummaryResponse struct {
	BatchID   uuid.UUID `json:"batchId"`
	Total     int       `json:"total"`
	Pending   int       `json:"pending"`
	Delivered int       `json:"delivered"`
	Failed    int       `json:"failed"`
	Cancelled int       `json:"cancelled"`
}

func batchSummaryToResponse(s app.BatchSummary) batchSummaryResponse {
	return batchSummaryResponse{
		BatchID:   s.ID,
		Total:     s.Total,
		Pending:   s.Pending,
		Delivered: s.Delivered,
		Failed:    s.Failed,
		Cancelled: s.Cancelled,
	}
}

// ---- list -------------------------------------------------------------------

type listResponse struct {
	Items []notificationResponse `json:"items"`
	Page  int                    `json:"page"`
	Limit int                    `json:"limit"`
	Total int                    `json:"total"`
}

// ---- cancel -----------------------------------------------------------------

type cancelResponse struct {
	MessageID uuid.UUID     `json:"messageId"`
	Status    domain.Status `json:"status"`
}

// ---- error ------------------------------------------------------------------

type errorResponse struct {
	Error string `json:"error"`
}

type validationErrorResponse struct {
	Errors []string `json:"errors"`
}
