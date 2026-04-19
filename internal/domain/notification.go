package domain

import (
	"time"

	"github.com/google/uuid"
)

type Channel string

const (
	ChannelSMS   Channel = "sms"
	ChannelEmail Channel = "email"
	ChannelPush  Channel = "push"
)

func (c Channel) Valid() bool {
	switch c {
	case ChannelSMS, ChannelEmail, ChannelPush:
		return true
	}
	return false
}

type Priority string

const (
	PriorityHigh   Priority = "high"
	PriorityNormal Priority = "normal"
	PriorityLow    Priority = "low"
)

func (p Priority) Valid() bool {
	switch p {
	case PriorityHigh, PriorityNormal, PriorityLow:
		return true
	}
	return false
}

type Status string

const (
	StatusPending   Status = "pending"
	StatusDelivered Status = "delivered"
	StatusFailed    Status = "failed"
	StatusCancelled Status = "cancelled"
)

func (s Status) Valid() bool {
	switch s {
	case StatusPending, StatusDelivered, StatusFailed, StatusCancelled:
		return true
	}
	return false
}

type Notification struct {
	ID             uuid.UUID
	Recipient      string
	Channel        Channel
	Content        string
	Priority       Priority
	Status         Status
	CorrelationID  uuid.UUID
	BatchID        *uuid.UUID
	IdempotencyKey *string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}
