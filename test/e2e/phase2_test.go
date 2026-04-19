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

// ---- Phase 2 response types -------------------------------------------------

type batchCreateResponse struct {
	BatchID   string `json:"batchId"`
	Accepted  int    `json:"accepted"`
	Timestamp string `json:"timestamp"`
}

type batchSummaryResponse struct {
	BatchID   string `json:"batchId"`
	Total     int    `json:"total"`
	Pending   int    `json:"pending"`
	Delivered int    `json:"delivered"`
	Failed    int    `json:"failed"`
	Cancelled int    `json:"cancelled"`
}

type listResponse struct {
	Items []notificationGetResponse `json:"items"`
	Page  int                       `json:"page"`
	Limit int                       `json:"limit"`
	Total int                       `json:"total"`
}

type cancelResponse struct {
	MessageID string `json:"messageId"`
	Status    string `json:"status"`
}

// ---- helpers ----------------------------------------------------------------

func postBatch(t *testing.T, baseURL, jsonBody string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, baseURL+"/notifications/batch", strings.NewReader(jsonBody))
	if err != nil {
		t.Fatalf("build batch request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /notifications/batch: %v", err)
	}
	return resp
}

func getBatchSummary(t *testing.T, baseURL, batchID string) *http.Response {
	t.Helper()
	resp, err := http.Get(fmt.Sprintf("%s/notifications/batch/%s", baseURL, batchID))
	if err != nil {
		t.Fatalf("GET /notifications/batch/%s: %v", batchID, err)
	}
	return resp
}

func deleteNotification(t *testing.T, baseURL, id string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, baseURL+"/notifications/"+id, nil)
	if err != nil {
		t.Fatalf("build delete request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE /notifications/%s: %v", id, err)
	}
	return resp
}

func postNotificationWithKey(t *testing.T, baseURL, jsonBody, idempotencyKey string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, baseURL+"/notifications", strings.NewReader(jsonBody))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /notifications: %v", err)
	}
	return resp
}

// pollBatchSummary polls GET /notifications/batch/{id} until all total notifications
// have a non-pending status (delivered+failed+cancelled == total) or the deadline passes.
func pollBatchAllSettled(t *testing.T, baseURL, batchID string, total int, deadline time.Duration) batchSummaryResponse {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()

	for {
		resp, err := http.Get(fmt.Sprintf("%s/notifications/batch/%s", baseURL, batchID))
		if err != nil {
			t.Fatalf("GET batch summary: %v", err)
		}
		var summary batchSummaryResponse
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		_ = json.Unmarshal(body, &summary)

		settled := summary.Delivered + summary.Failed + summary.Cancelled
		if settled >= total {
			return summary
		}

		select {
		case <-ctx.Done():
			t.Fatalf("pollBatchAllSettled: timed out after %s; last summary: %+v", deadline, summary)
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// ---- tests ------------------------------------------------------------------

func TestPhase2(t *testing.T) {
	h := newHarness(t)
	t.Cleanup(h.close)

	// -------------------------------------------------------------------------
	t.Run("Batch_HappyPath", func(t *testing.T) {
		h.resetFull(t)

		var webhookHits int32
		webhookSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			atomic.AddInt32(&webhookHits, 1)
			w.WriteHeader(http.StatusOK)
		}))
		defer webhookSrv.Close()

		apiSrv := h.startAPI(t, webhookSrv.URL)
		defer apiSrv.Close()
		workerCancel := h.startWorker(t, webhookSrv.URL)
		defer workerCancel()

		body := `{"notifications":[
			{"to":"+905551234567","channel":"sms","content":"batch msg 1"},
			{"to":"user@example.com","channel":"email","content":"batch msg 2"},
			{"to":"+905559876543","channel":"sms","content":"batch msg 3"}
		]}`

		resp := postBatch(t, apiSrv.URL, body)
		if resp.StatusCode != http.StatusAccepted {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			t.Fatalf("batch create status = %d, want 202; body: %s", resp.StatusCode, b)
		}

		var batchResp batchCreateResponse
		decodeBody(t, resp, &batchResp)

		assertUUID(t, "batchId", batchResp.BatchID)
		if batchResp.Accepted != 3 {
			t.Errorf("accepted = %d, want 3", batchResp.Accepted)
		}
		assertRFC3339(t, "timestamp", batchResp.Timestamp)

		// Wait for all 3 to be delivered.
		summary := pollBatchAllSettled(t, apiSrv.URL, batchResp.BatchID, 3, 30*time.Second)

		if summary.Total != 3 {
			t.Errorf("summary.total = %d, want 3", summary.Total)
		}
		if summary.Delivered != 3 {
			t.Errorf("summary.delivered = %d, want 3", summary.Delivered)
		}
		if summary.Pending != 0 {
			t.Errorf("summary.pending = %d, want 0", summary.Pending)
		}

		if atomic.LoadInt32(&webhookHits) != 3 {
			t.Errorf("webhook hits = %d, want 3", atomic.LoadInt32(&webhookHits))
		}
	})

	// -------------------------------------------------------------------------
	t.Run("Batch_ValidationError", func(t *testing.T) {
		h.resetFull(t)

		webhookSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer webhookSrv.Close()

		apiSrv := h.startAPI(t, webhookSrv.URL)
		defer apiSrv.Close()

		// Item 1 has a bad phone number.
		body := `{"notifications":[
			{"to":"+905551234567","channel":"sms","content":"ok"},
			{"to":"not-a-phone","channel":"sms","content":"bad recipient"}
		]}`

		resp := postBatch(t, apiSrv.URL, body)
		if resp.StatusCode != http.StatusBadRequest {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			t.Fatalf("status = %d, want 400; body: %s", resp.StatusCode, b)
		}

		var errBody map[string]any
		decodeBody(t, resp, &errBody)
		if errBody["errors"] == nil {
			t.Errorf("expected errors field in 400 body, got %v", errBody)
		}

		// Nothing persisted.
		var count int
		_ = h.pool.QueryRow(context.Background(), "SELECT COUNT(*) FROM notifications").Scan(&count)
		if count != 0 {
			t.Errorf("expected 0 rows after validation error, got %d", count)
		}
		var batchCount int
		_ = h.pool.QueryRow(context.Background(), "SELECT COUNT(*) FROM batches").Scan(&batchCount)
		if batchCount != 0 {
			t.Errorf("expected 0 batch rows after validation error, got %d", batchCount)
		}
	})

	// -------------------------------------------------------------------------
	t.Run("Batch_SizeCap", func(t *testing.T) {
		h.resetFull(t)

		apiSrv := h.startAPI(t, "http://localhost:1") // webhook URL unused
		defer apiSrv.Close()

		// Build a payload with 1001 minimal notifications.
		var sb strings.Builder
		sb.WriteString(`{"notifications":[`)
		for i := 0; i < 1001; i++ {
			if i > 0 {
				sb.WriteByte(',')
			}
			sb.WriteString(`{"to":"user@example.com","channel":"email","content":"x"}`)
		}
		sb.WriteString(`]}`)

		resp := postBatch(t, apiSrv.URL, sb.String())
		if resp.StatusCode != http.StatusBadRequest {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			t.Fatalf("status = %d, want 400 for 1001 items; body: %s", resp.StatusCode, b)
		}
		resp.Body.Close()
	})

	// -------------------------------------------------------------------------
	t.Run("List_PaginationAndFilters", func(t *testing.T) {
		h.resetFull(t)

		webhookSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			// Return 500 so all stay pending/failed — don't deliver to keep
			// status controlled. Worker marks them failed.
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer webhookSrv.Close()

		apiSrv := h.startAPI(t, webhookSrv.URL)
		defer apiSrv.Close()
		workerCancel := h.startWorker(t, webhookSrv.URL)
		defer workerCancel()

		// Seed 3 SMS and 2 email notifications.
		for i := 0; i < 3; i++ {
			resp := postNotification(t, apiSrv.URL,
				fmt.Sprintf(`{"to":"+90555123456%d","channel":"sms","content":"sms %d"}`, i, i), "")
			resp.Body.Close()
		}
		for i := 0; i < 2; i++ {
			resp := postNotification(t, apiSrv.URL,
				fmt.Sprintf(`{"to":"user%d@example.com","channel":"email","content":"email %d"}`, i, i), "")
			resp.Body.Close()
		}

		// Wait for all 5 to be processed (move out of pending).
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		for {
			var pending int
			_ = h.pool.QueryRow(ctx, "SELECT COUNT(*) FROM notifications WHERE status = 'pending'").Scan(&pending)
			if pending == 0 {
				break
			}
			select {
			case <-ctx.Done():
				t.Fatal("timed out waiting for notifications to leave pending")
			case <-time.After(200 * time.Millisecond):
			}
		}

		// GET ?channel=sms&limit=2&page=1
		resp, err := http.Get(fmt.Sprintf("%s/notifications?channel=sms&limit=2&page=1", apiSrv.URL))
		if err != nil {
			t.Fatalf("GET /notifications: %v", err)
		}
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			t.Fatalf("list status = %d, want 200; body: %s", resp.StatusCode, b)
		}
		var listResp listResponse
		decodeBody(t, resp, &listResp)

		if listResp.Total != 3 {
			t.Errorf("total = %d, want 3 (sms only)", listResp.Total)
		}
		if len(listResp.Items) != 2 {
			t.Errorf("items count = %d, want 2 (limit=2)", len(listResp.Items))
		}
		if listResp.Page != 1 {
			t.Errorf("page = %d, want 1", listResp.Page)
		}
		if listResp.Limit != 2 {
			t.Errorf("limit = %d, want 2", listResp.Limit)
		}
		for _, item := range listResp.Items {
			if item.Channel != "sms" {
				t.Errorf("item channel = %q, want sms", item.Channel)
			}
		}

		// GET page 2 — should have 1 remaining SMS.
		resp2, err := http.Get(fmt.Sprintf("%s/notifications?channel=sms&limit=2&page=2", apiSrv.URL))
		if err != nil {
			t.Fatalf("GET /notifications page 2: %v", err)
		}
		var listResp2 listResponse
		decodeBody(t, resp2, &listResp2)
		if len(listResp2.Items) != 1 {
			t.Errorf("page 2 items = %d, want 1", len(listResp2.Items))
		}

		// GET with invalid status → 400.
		resp3, err := http.Get(fmt.Sprintf("%s/notifications?status=bogus", apiSrv.URL))
		if err != nil {
			t.Fatalf("GET /notifications bogus status: %v", err)
		}
		resp3.Body.Close()
		if resp3.StatusCode != http.StatusBadRequest {
			t.Errorf("invalid status param: got %d, want 400", resp3.StatusCode)
		}
	})

	// -------------------------------------------------------------------------
	t.Run("Cancel_Pending", func(t *testing.T) {
		h.resetFull(t)

		// Use a webhook that blocks forever so the notification stays pending.
		// We start the API but do NOT start the worker — the message sits in
		// RabbitMQ. The row remains in pending status in the DB.
		webhookSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer webhookSrv.Close()

		apiSrv := h.startAPI(t, webhookSrv.URL)
		defer apiSrv.Close()
		// Intentionally no startWorker — notification stays pending in DB.

		resp := postNotification(t, apiSrv.URL,
			`{"to":"+905551234567","channel":"sms","content":"cancel me"}`, "")
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("create status = %d, want 202", resp.StatusCode)
		}
		var createBody createNotificationResponse
		decodeBody(t, resp, &createBody)

		// Assert the row is pending.
		var status string
		_ = h.pool.QueryRow(context.Background(),
			"SELECT status FROM notifications WHERE id = $1", createBody.MessageID,
		).Scan(&status)
		if status != "pending" {
			t.Fatalf("expected pending before cancel, got %q", status)
		}

		// Cancel → 200 with cancelled status.
		delResp := deleteNotification(t, apiSrv.URL, createBody.MessageID)
		if delResp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(delResp.Body)
			delResp.Body.Close()
			t.Fatalf("cancel status = %d, want 200; body: %s", delResp.StatusCode, b)
		}
		var cancelBody cancelResponse
		decodeBody(t, delResp, &cancelBody)

		if cancelBody.MessageID != createBody.MessageID {
			t.Errorf("cancel messageId = %q, want %q", cancelBody.MessageID, createBody.MessageID)
		}
		if cancelBody.Status != "cancelled" {
			t.Errorf("cancel status = %q, want cancelled", cancelBody.Status)
		}

		// Cancel again → 409.
		delResp2 := deleteNotification(t, apiSrv.URL, createBody.MessageID)
		defer delResp2.Body.Close()
		if delResp2.StatusCode != http.StatusConflict {
			t.Errorf("second cancel status = %d, want 409", delResp2.StatusCode)
		}
	})

	// -------------------------------------------------------------------------
	t.Run("Cancel_NonPending_Returns409", func(t *testing.T) {
		h.resetFull(t)

		var webhookHits int32
		webhookSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			atomic.AddInt32(&webhookHits, 1)
			w.WriteHeader(http.StatusOK)
		}))
		defer webhookSrv.Close()

		apiSrv := h.startAPI(t, webhookSrv.URL)
		defer apiSrv.Close()
		workerCancel := h.startWorker(t, webhookSrv.URL)
		defer workerCancel()

		resp := postNotification(t, apiSrv.URL,
			`{"to":"+905551234567","channel":"sms","content":"deliver me then cancel"}`, "")
		var createBody createNotificationResponse
		decodeBody(t, resp, &createBody)

		// Wait until delivered.
		finalStatus := h.pollStatus(t, createBody.MessageID, 15*time.Second)
		if finalStatus != "delivered" {
			t.Fatalf("expected delivered, got %q", finalStatus)
		}

		// Attempt cancel → 409.
		delResp := deleteNotification(t, apiSrv.URL, createBody.MessageID)
		defer delResp.Body.Close()
		if delResp.StatusCode != http.StatusConflict {
			t.Errorf("cancel delivered: status = %d, want 409", delResp.StatusCode)
		}
	})

	// -------------------------------------------------------------------------
	t.Run("Cancel_NotFound_Returns404", func(t *testing.T) {
		h.resetFull(t)

		apiSrv := h.startAPI(t, "http://localhost:1")
		defer apiSrv.Close()

		delResp := deleteNotification(t, apiSrv.URL, uuid.NewString())
		defer delResp.Body.Close()
		if delResp.StatusCode != http.StatusNotFound {
			t.Errorf("cancel unknown id: status = %d, want 404", delResp.StatusCode)
		}
	})

	// -------------------------------------------------------------------------
	t.Run("Idempotency_SameKey_SameResponse", func(t *testing.T) {
		h.resetFull(t)

		var webhookHits int32
		webhookSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			atomic.AddInt32(&webhookHits, 1)
			w.WriteHeader(http.StatusOK)
		}))
		defer webhookSrv.Close()

		apiSrv := h.startAPI(t, webhookSrv.URL)
		defer apiSrv.Close()
		workerCancel := h.startWorker(t, webhookSrv.URL)
		defer workerCancel()

		body := `{"to":"+905551234567","channel":"sms","content":"idempotent"}`
		key := "idem-key-" + uuid.NewString()

		resp1 := postNotificationWithKey(t, apiSrv.URL, body, key)
		if resp1.StatusCode != http.StatusAccepted {
			b, _ := io.ReadAll(resp1.Body)
			resp1.Body.Close()
			t.Fatalf("first POST status = %d, want 202; body: %s", resp1.StatusCode, b)
		}
		var r1 createNotificationResponse
		decodeBody(t, resp1, &r1)
		assertUUID(t, "messageId", r1.MessageID)

		resp2 := postNotificationWithKey(t, apiSrv.URL, body, key)
		if resp2.StatusCode != http.StatusAccepted {
			b, _ := io.ReadAll(resp2.Body)
			resp2.Body.Close()
			t.Fatalf("second POST status = %d, want 202; body: %s", resp2.StatusCode, b)
		}
		var r2 createNotificationResponse
		decodeBody(t, resp2, &r2)

		if r1.MessageID != r2.MessageID {
			t.Errorf("idempotency: messageIds differ: %q vs %q", r1.MessageID, r2.MessageID)
		}

		// Only one row in DB.
		var count int
		_ = h.pool.QueryRow(context.Background(), "SELECT COUNT(*) FROM notifications").Scan(&count)
		if count != 1 {
			t.Errorf("expected 1 DB row, got %d", count)
		}

		// Wait for delivery (only 1 webhook call expected since replay skips publish).
		h.pollStatus(t, r1.MessageID, 15*time.Second)
		if atomic.LoadInt32(&webhookHits) != 1 {
			t.Errorf("webhook hits = %d, want 1 (replay must not re-publish)", atomic.LoadInt32(&webhookHits))
		}
	})
}
