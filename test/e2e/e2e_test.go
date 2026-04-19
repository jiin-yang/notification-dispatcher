//go:build e2e

package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

// createNotificationResponse mirrors the 202 response shape.
type createNotificationResponse struct {
	MessageID string `json:"messageId"`
	Status    string `json:"status"`
	Timestamp string `json:"timestamp"`
}

// notificationGetResponse mirrors the GET /notifications/{id} response shape.
type notificationGetResponse struct {
	MessageID     string `json:"messageId"`
	Recipient     string `json:"recipient"`
	Channel       string `json:"channel"`
	Content       string `json:"content"`
	Priority      string `json:"priority"`
	Status        string `json:"status"`
	CorrelationID string `json:"correlationId"`
	CreatedAt     string `json:"createdAt"`
	UpdatedAt     string `json:"updatedAt"`
}

// webhookBody is what the worker POSTs to the webhook server.
type webhookBody struct {
	NotificationID string `json:"notification_id"`
	Recipient      string `json:"recipient"`
	Channel        string `json:"channel"`
	Content        string `json:"content"`
	CorrelationID  string `json:"correlation_id"`
}

// TestE2E is the root suite. A single harness is set up once; individual
// subtests truncate the table before each run so they are isolated.
func TestE2E(t *testing.T) {
	h := newHarness(t)
	t.Cleanup(h.close)

	t.Run("HappyPath_SMS_Delivered", func(t *testing.T) {
		h.reset(t)

		var capturedWebhook webhookBody
		var webhookHits int32
		webhookSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&webhookHits, 1)
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &capturedWebhook)
			w.WriteHeader(http.StatusOK)
		}))
		defer webhookSrv.Close()

		apiSrv := h.startAPI(t, webhookSrv.URL)
		defer apiSrv.Close()
		workerCancel := h.startWorker(t, webhookSrv.URL)
		defer workerCancel()

		correlationID := uuid.NewString()
		resp := postNotification(t, apiSrv.URL, `{"to":"+905551234567","channel":"sms","content":"Your message"}`, correlationID)

		// Assert 202 + response contract.
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("status = %d, want 202", resp.StatusCode)
		}
		var body createNotificationResponse
		decodeBody(t, resp, &body)

		assertUUID(t, "messageId", body.MessageID)
		if body.Status != "accepted" {
			t.Errorf("status = %q, want accepted", body.Status)
		}
		assertRFC3339(t, "timestamp", body.Timestamp)

		// Response header must echo the correlation ID we sent.
		gotCorr := resp.Header.Get("X-Correlation-ID")
		if gotCorr != correlationID {
			t.Errorf("response X-Correlation-ID = %q, want %q", gotCorr, correlationID)
		}

		// Assert DB row persisted correctly.
		assertDBRow(t, h, body.MessageID, "+905551234567", "sms", "Your message")

		// Wait for worker to deliver.
		status := h.pollStatus(t, body.MessageID, 15*time.Second)
		if status != "delivered" {
			t.Errorf("final DB status = %q, want delivered", status)
		}

		// Assert webhook received exactly one request.
		if atomic.LoadInt32(&webhookHits) != 1 {
			t.Errorf("webhook hits = %d, want 1", atomic.LoadInt32(&webhookHits))
		}

		// Assert webhook body shape and correlation ID propagation.
		assertWebhookBody(t, capturedWebhook, body.MessageID, "+905551234567", "sms", "Your message")
		if capturedWebhook.CorrelationID != correlationID {
			t.Errorf("webhook correlation_id = %q, want %q", capturedWebhook.CorrelationID, correlationID)
		}
	})

	t.Run("Webhook5xx_Terminal_Failed", func(t *testing.T) {
		h.reset(t)

		webhookSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer webhookSrv.Close()

		apiSrv := h.startAPI(t, webhookSrv.URL)
		defer apiSrv.Close()
		workerCancel := h.startWorker(t, webhookSrv.URL)
		defer workerCancel()

		resp := postNotification(t, apiSrv.URL, `{"to":"+905559999999","channel":"sms","content":"fail me"}`, "")
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("status = %d, want 202", resp.StatusCode)
		}
		var body createNotificationResponse
		decodeBody(t, resp, &body)

		status := h.pollStatus(t, body.MessageID, 15*time.Second)
		if status != "failed" {
			t.Errorf("final DB status = %q, want failed", status)
		}
	})

	t.Run("ValidationError_BadPhone", func(t *testing.T) {
		h.reset(t)

		var webhookHits int32
		webhookSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			atomic.AddInt32(&webhookHits, 1)
			w.WriteHeader(http.StatusOK)
		}))
		defer webhookSrv.Close()

		apiSrv := h.startAPI(t, webhookSrv.URL)
		defer apiSrv.Close()

		resp := postNotification(t, apiSrv.URL, `{"to":"not-a-phone","channel":"sms","content":"x"}`, "")
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", resp.StatusCode)
		}

		// Response body must contain an error field.
		var errBody map[string]string
		decodeBody(t, resp, &errBody)
		if errBody["error"] == "" {
			t.Errorf("expected non-empty error field in 400 body, got %v", errBody)
		}

		// Nothing should be persisted.
		var count int
		err := h.pool.QueryRow(context.Background(), "SELECT COUNT(*) FROM notifications").Scan(&count)
		if err != nil {
			t.Fatalf("count query: %v", err)
		}
		if count != 0 {
			t.Errorf("expected 0 rows after validation error, got %d", count)
		}

		// Webhook must not have been called.
		if atomic.LoadInt32(&webhookHits) != 0 {
			t.Errorf("webhook should not be called on validation error, got %d hits", atomic.LoadInt32(&webhookHits))
		}
	})

	t.Run("HappyPath_Email_Delivered", func(t *testing.T) {
		h.reset(t)

		var capturedWebhook webhookBody
		webhookSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &capturedWebhook)
			w.WriteHeader(http.StatusOK)
		}))
		defer webhookSrv.Close()

		apiSrv := h.startAPI(t, webhookSrv.URL)
		defer apiSrv.Close()
		workerCancel := h.startWorker(t, webhookSrv.URL)
		defer workerCancel()

		resp := postNotification(t, apiSrv.URL,
			`{"to":"user@example.com","channel":"email","content":"Hello email"}`, "")
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("status = %d, want 202", resp.StatusCode)
		}
		var body createNotificationResponse
		decodeBody(t, resp, &body)

		assertUUID(t, "messageId", body.MessageID)
		assertRFC3339(t, "timestamp", body.Timestamp)
		assertDBRow(t, h, body.MessageID, "user@example.com", "email", "Hello email")

		status := h.pollStatus(t, body.MessageID, 15*time.Second)
		if status != "delivered" {
			t.Errorf("final DB status = %q, want delivered", status)
		}
		assertWebhookBody(t, capturedWebhook, body.MessageID, "user@example.com", "email", "Hello email")
	})

	t.Run("GetNotification_DTOShape", func(t *testing.T) {
		h.reset(t)

		webhookSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer webhookSrv.Close()

		apiSrv := h.startAPI(t, webhookSrv.URL)
		defer apiSrv.Close()
		workerCancel := h.startWorker(t, webhookSrv.URL)
		defer workerCancel()

		correlationID := uuid.NewString()
		postResp := postNotification(t, apiSrv.URL, `{"to":"+905551234567","channel":"sms","content":"Smoke test"}`, correlationID)
		var postBody createNotificationResponse
		decodeBody(t, postResp, &postBody)

		// Wait for delivery so the GET shows final state.
		h.pollStatus(t, postBody.MessageID, 15*time.Second)

		getResp, err := http.Get(fmt.Sprintf("%s/notifications/%s", apiSrv.URL, postBody.MessageID))
		if err != nil {
			t.Fatalf("GET /notifications/{id}: %v", err)
		}
		defer getResp.Body.Close()

		if getResp.StatusCode != http.StatusOK {
			t.Fatalf("GET status = %d, want 200", getResp.StatusCode)
		}
		var dto notificationGetResponse
		decodeBody(t, getResp, &dto)

		if dto.MessageID != postBody.MessageID {
			t.Errorf("messageId = %q, want %q", dto.MessageID, postBody.MessageID)
		}
		if dto.Recipient != "+905551234567" {
			t.Errorf("recipient = %q, want +905551234567", dto.Recipient)
		}
		if dto.Channel != "sms" {
			t.Errorf("channel = %q, want sms", dto.Channel)
		}
		if dto.Content != "Smoke test" {
			t.Errorf("content = %q, want Smoke test", dto.Content)
		}
		if dto.Status != "delivered" {
			t.Errorf("status = %q, want delivered", dto.Status)
		}
		assertUUID(t, "correlationId", dto.CorrelationID)
		if dto.CorrelationID != correlationID {
			t.Errorf("correlationId = %q, want %q", dto.CorrelationID, correlationID)
		}
	})

	t.Run("CorrelationID_GeneratedWhenAbsent", func(t *testing.T) {
		h.reset(t)

		var capturedWebhook webhookBody
		webhookSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &capturedWebhook)
			w.WriteHeader(http.StatusOK)
		}))
		defer webhookSrv.Close()

		apiSrv := h.startAPI(t, webhookSrv.URL)
		defer apiSrv.Close()
		workerCancel := h.startWorker(t, webhookSrv.URL)
		defer workerCancel()

		// POST without X-Correlation-ID header.
		resp := postNotification(t, apiSrv.URL, `{"to":"+905551234567","channel":"sms","content":"no corr id"}`, "")
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("status = %d, want 202", resp.StatusCode)
		}

		generatedCorr := resp.Header.Get("X-Correlation-ID")
		assertUUID(t, "response X-Correlation-ID", generatedCorr)

		// Wait for delivery.
		var body createNotificationResponse
		decodeBody(t, resp, &body)
		h.pollStatus(t, body.MessageID, 15*time.Second)

		// Webhook body's correlation_id must match the generated header value.
		if capturedWebhook.CorrelationID != generatedCorr {
			t.Errorf("webhook correlation_id = %q, want %q (generated)", capturedWebhook.CorrelationID, generatedCorr)
		}
	})
}

// ---- helpers ----------------------------------------------------------------

func postNotification(t *testing.T, baseURL, jsonBody, correlationID string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, baseURL+"/notifications", strings.NewReader(jsonBody))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if correlationID != "" {
		req.Header.Set("X-Correlation-ID", correlationID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /notifications: %v", err)
	}
	return resp
}

func decodeBody(t *testing.T, resp *http.Response, dst any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
		t.Fatalf("decode response body: %v", err)
	}
}

func assertUUID(t *testing.T, field, value string) {
	t.Helper()
	if _, err := uuid.Parse(value); err != nil {
		t.Errorf("%s = %q is not a valid UUID: %v", field, value, err)
	}
}

func assertRFC3339(t *testing.T, field, value string) {
	t.Helper()
	if _, err := time.Parse(time.RFC3339, value); err != nil {
		t.Errorf("%s = %q is not valid RFC3339: %v", field, value, err)
	}
}

func assertDBRow(t *testing.T, h *harness, id, recipient, channel, content string) {
	t.Helper()
	var gotRecipient, gotChannel, gotContent string
	err := h.pool.QueryRow(context.Background(),
		"SELECT recipient, channel, content FROM notifications WHERE id = $1", id,
	).Scan(&gotRecipient, &gotChannel, &gotContent)
	if err != nil {
		t.Fatalf("query notification %s: %v", id, err)
	}
	if gotRecipient != recipient {
		t.Errorf("DB recipient = %q, want %q", gotRecipient, recipient)
	}
	if gotChannel != channel {
		t.Errorf("DB channel = %q, want %q", gotChannel, channel)
	}
	if gotContent != content {
		t.Errorf("DB content = %q, want %q", gotContent, content)
	}
}

func assertWebhookBody(t *testing.T, wb webhookBody, notificationID, recipient, channel, content string) {
	t.Helper()
	if wb.NotificationID != notificationID {
		t.Errorf("webhook notification_id = %q, want %q", wb.NotificationID, notificationID)
	}
	if wb.Recipient != recipient {
		t.Errorf("webhook recipient = %q, want %q", wb.Recipient, recipient)
	}
	if wb.Channel != channel {
		t.Errorf("webhook channel = %q, want %q", wb.Channel, channel)
	}
	if wb.Content != content {
		t.Errorf("webhook content = %q, want %q", wb.Content, content)
	}
}
