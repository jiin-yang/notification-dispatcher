package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/jiin-yang/notification-dispatcher/internal/domain"
)

type WebhookProvider struct {
	url    string
	client *http.Client
}

type WebhookOptions struct {
	URL     string
	Timeout time.Duration
}

func NewWebhookProvider(opts WebhookOptions) *WebhookProvider {
	if opts.Timeout <= 0 {
		opts.Timeout = 10 * time.Second
	}
	return &WebhookProvider{
		url:    opts.URL,
		client: &http.Client{Timeout: opts.Timeout},
	}
}

type webhookPayload struct {
	NotificationID string `json:"notification_id"`
	Recipient      string `json:"recipient"`
	Channel        string `json:"channel"`
	Content        string `json:"content"`
	CorrelationID  string `json:"correlation_id"`
}

// Send posts the notification to the configured webhook URL. Any 2xx status is
// treated as delivered; the response body is intentionally discarded per the
// Phase 1 contract.
func (p *WebhookProvider) Send(ctx context.Context, n domain.Notification) error {
	body, err := json.Marshal(webhookPayload{
		NotificationID: n.ID.String(),
		Recipient:      n.Recipient,
		Channel:        string(n.Channel),
		Content:        n.Content,
		CorrelationID:  n.CorrelationID.String(),
	})
	if err != nil {
		return fmt.Errorf("marshal webhook payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build webhook request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		// A cancelled caller context isn't a terminal delivery failure — the
		// broker should requeue the message (on channel close) rather than
		// mark the row failed. Return the raw ctx error so deliver.go can
		// distinguish this from a real transport failure.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf("%w: webhook transport: %v", domain.ErrDeliveryFailed, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%w: webhook status %d", domain.ErrDeliveryFailed, resp.StatusCode)
	}
	return nil
}
