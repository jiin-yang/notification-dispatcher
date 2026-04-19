package postgres

import (
	"time"

	"github.com/google/uuid"
)

// DeliveryAttemptRow is a scan target for delivery_attempts rows. It lives in
// the postgres adapter (not domain/app) because it maps 1:1 to the DB schema
// and has no business behaviour.
type DeliveryAttemptRow struct {
	ID               int64
	NotificationID   uuid.UUID
	AttemptNumber    int
	Status           string
	ErrorReason      *string
	ProviderResponse *string
	AttemptedAt      time.Time
}
