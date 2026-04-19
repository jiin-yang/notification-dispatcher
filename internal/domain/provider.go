package domain

import (
	"context"
	"errors"
)

// ErrDeliveryFailed signals a terminal delivery failure (non-2xx from the
// downstream channel). Phase 1 treats this as final; Phase 4 will re-route to
// the retry exchange.
var ErrDeliveryFailed = errors.New("delivery failed")

type Provider interface {
	Send(ctx context.Context, n Notification) error
}
