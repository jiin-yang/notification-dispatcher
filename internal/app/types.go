package app

import (
	"time"

	"github.com/google/uuid"

	"github.com/jiin-yang/notification-dispatcher/internal/domain"
)

// BatchSummary is the application-layer result for GET /notifications/batch/{id}.
type BatchSummary struct {
	ID        uuid.UUID
	Total     int
	Pending   int
	Delivered int
	Failed    int
	Cancelled int
}

// ListFilter holds optional filter parameters for listing notifications.
// Zero values mean "no filter" for that field.
type ListFilter struct {
	Status   domain.Status
	Channel  domain.Channel
	DateFrom time.Time
	DateTo   time.Time
}
