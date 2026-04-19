package provider_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jiin-yang/notification-dispatcher/internal/adapter/provider"
	"github.com/jiin-yang/notification-dispatcher/internal/domain"
)

func sampleNotification() domain.Notification {
	return domain.Notification{
		ID:            uuid.MustParse("d490fd94-60b3-4ead-9543-642bfc076f58"),
		Recipient:     "+905551234567",
		Channel:       domain.ChannelSMS,
		Content:       "Smoke test",
		Priority:      domain.PriorityNormal,
		Status:        domain.StatusPending,
		CorrelationID: uuid.MustParse("31f4cea6-68c6-4cfb-88d2-7ad36a5f1e16"),
	}
}

func TestWebhookProvider_Send_Success(t *testing.T) {
	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("content-type = %q, want application/json", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ignored":"body"}`)
	}))
	defer srv.Close()

	p := provider.NewWebhookProvider(provider.WebhookOptions{URL: srv.URL, Timeout: 2 * time.Second})
	if err := p.Send(context.Background(), sampleNotification()); err != nil {
		t.Fatalf("Send returned %v", err)
	}

	wantKeys := []string{"notification_id", "recipient", "channel", "content", "correlation_id"}
	for _, k := range wantKeys {
		if _, ok := captured[k]; !ok {
			t.Errorf("payload missing key %q; got %v", k, captured)
		}
	}
	if captured["channel"] != "sms" {
		t.Errorf("channel = %v, want sms", captured["channel"])
	}
}

func TestWebhookProvider_Send_Non2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	p := provider.NewWebhookProvider(provider.WebhookOptions{URL: srv.URL, Timeout: 2 * time.Second})
	err := p.Send(context.Background(), sampleNotification())
	if !errors.Is(err, domain.ErrDeliveryFailed) {
		t.Fatalf("want ErrDeliveryFailed, got %v", err)
	}
}

func TestWebhookProvider_Send_TransportError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := provider.NewWebhookProvider(provider.WebhookOptions{URL: srv.URL, Timeout: 50 * time.Millisecond})
	err := p.Send(context.Background(), sampleNotification())
	if !errors.Is(err, domain.ErrDeliveryFailed) {
		t.Fatalf("want ErrDeliveryFailed on timeout, got %v", err)
	}
}
