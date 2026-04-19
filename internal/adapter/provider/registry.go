package provider

import (
	"context"
	"fmt"

	"github.com/jiin-yang/notification-dispatcher/internal/domain"
)

// Registry implements domain.Provider by dispatching Send to the provider
// registered for the notification's channel. Wire all three channels to
// WebhookProvider in Phase 3; swap individual entries in later phases when
// real provider integrations are added.
type Registry struct {
	providers map[domain.Channel]domain.Provider
}

// NewRegistry returns an empty registry. Use Register or MustRegister to
// populate it before use.
func NewRegistry() *Registry {
	return &Registry{providers: make(map[domain.Channel]domain.Provider)}
}

// MustRegister adds p for channel ch. Panics if ch is already registered,
// which catches wiring mistakes at startup rather than at message-processing
// time.
func (r *Registry) MustRegister(ch domain.Channel, p domain.Provider) {
	if _, exists := r.providers[ch]; exists {
		panic(fmt.Sprintf("provider already registered for channel %q", ch))
	}
	r.providers[ch] = p
}

// Send looks up the provider for n.Channel and delegates to it. Returns a
// non-nil error if no provider is registered for that channel.
func (r *Registry) Send(ctx context.Context, n domain.Notification) error {
	p, ok := r.providers[n.Channel]
	if !ok {
		return fmt.Errorf("no provider registered for channel %q", n.Channel)
	}
	return p.Send(ctx, n)
}
