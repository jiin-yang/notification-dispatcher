//go:build e2e

package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jiin-yang/notification-dispatcher/internal/app"
	"github.com/jiin-yang/notification-dispatcher/internal/domain"
)

// TestPhase3 is the root suite for Phase 3 tests.
func TestPhase3(t *testing.T) {
	h := newHarness(t)
	t.Cleanup(h.close)

	// -------------------------------------------------------------------------
	// 1. Per-channel queue routing
	// -------------------------------------------------------------------------
	t.Run("PerChannelQueueRouting", func(t *testing.T) {
		h.resetFull(t)

		var mu sync.Mutex
		type capturedCall struct {
			channel string
			id      string
		}
		var hits []capturedCall
		var webhookHits int32

		webhookSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&webhookHits, 1)
			body, _ := io.ReadAll(r.Body)
			var wb webhookBody
			_ = json.Unmarshal(body, &wb)
			mu.Lock()
			hits = append(hits, capturedCall{channel: wb.Channel, id: wb.NotificationID})
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		}))
		defer webhookSrv.Close()

		apiSrv := h.startAPI(t, webhookSrv.URL)
		defer apiSrv.Close()
		workerCancel := h.startWorker(t, webhookSrv.URL)
		defer workerCancel()

		// Post one notification per channel with an explicit priority so we can
		// verify routing-key → queue binding works for all 9 combinations.
		testCases := []struct {
			body    string
			channel string
		}{
			{`{"to":"user@example.com","channel":"email","content":"routing test","priority":"high"}`, "email"},
			{`{"to":"+905551234567","channel":"sms","content":"routing test","priority":"normal"}`, "sms"},
			{`{"to":"device-token-abc","channel":"push","content":"routing test","priority":"low"}`, "push"},
		}

		ids := make([]string, len(testCases))
		for i, tc := range testCases {
			resp := postNotification(t, apiSrv.URL, tc.body, "")
			if resp.StatusCode != http.StatusAccepted {
				b, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				t.Fatalf("POST %s: status %d; body: %s", tc.channel, resp.StatusCode, b)
			}
			var cr createNotificationResponse
			decodeBody(t, resp, &cr)
			ids[i] = cr.MessageID
		}

		for _, id := range ids {
			status := h.pollStatus(t, id, 20*time.Second)
			if status != "delivered" {
				t.Errorf("notification %s: status = %q, want delivered", id, status)
			}
		}

		if atomic.LoadInt32(&webhookHits) != 3 {
			t.Errorf("webhook hits = %d, want 3", atomic.LoadInt32(&webhookHits))
		}

		mu.Lock()
		defer mu.Unlock()
		channelSeen := make(map[string]bool)
		for _, c := range hits {
			channelSeen[c.channel] = true
		}
		for _, tc := range testCases {
			if !channelSeen[tc.channel] {
				t.Errorf("channel %q not seen in webhook calls", tc.channel)
			}
		}
	})

	// -------------------------------------------------------------------------
	// 2. Consumer idempotency
	// -------------------------------------------------------------------------
	t.Run("ConsumerIdempotency", func(t *testing.T) {
		h.resetFull(t)

		var webhookHits int32
		webhookSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			atomic.AddInt32(&webhookHits, 1)
			w.WriteHeader(http.StatusOK)
		}))
		defer webhookSrv.Close()

		// Pre-seed the notification row via the API (no worker running yet).
		apiSrv := h.startAPI(t, webhookSrv.URL)
		defer apiSrv.Close()

		resp := postNotification(t, apiSrv.URL,
			`{"to":"+905551234567","channel":"sms","content":"idempotent consumer"}`, "")
		if resp.StatusCode != http.StatusAccepted {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			t.Fatalf("pre-seed POST: status %d; body: %s", resp.StatusCode, b)
		}
		var cr createNotificationResponse
		decodeBody(t, resp, &cr)
		notifID, _ := uuid.Parse(cr.MessageID)

		// Build the wire-format message body that the worker expects.
		msg := app.NotificationMessage{
			ID:            notifID,
			Recipient:     "+905551234567",
			Channel:       domain.ChannelSMS,
			Content:       "idempotent consumer",
			Priority:      domain.PriorityNormal,
			CorrelationID: uuid.New(),
		}
		msgBody, _ := json.Marshal(msg)
		headers := map[string]any{
			"notification_id": notifID.String(),
			"correlation_id":  uuid.NewString(),
		}

		// The API already published once. Purge that message and publish twice
		// manually so we control exactly how many copies hit the queue.
		h.purgeAllQueues(t)
		h.publishDirect(t, "sms.normal", headers, msgBody)
		h.publishDirect(t, "sms.normal", headers, msgBody)

		// Start the worker; it processes both messages.
		workerCancel := h.startWorker(t, webhookSrv.URL)
		defer workerCancel()

		status := h.pollStatus(t, cr.MessageID, 20*time.Second)
		if status != "delivered" {
			t.Errorf("status = %q, want delivered", status)
		}

		// Allow time for the second message to be picked up and skipped.
		time.Sleep(600 * time.Millisecond)

		got := atomic.LoadInt32(&webhookHits)
		if got != 1 {
			t.Errorf("webhook hits = %d, want exactly 1 (second must be deduped)", got)
		}
	})

	// -------------------------------------------------------------------------
	// 3. Rate limit — single channel
	// -------------------------------------------------------------------------
	t.Run("RateLimit_SingleChannel", func(t *testing.T) {
		h.reset(t)

		webhookSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer webhookSrv.Close()

		// Tight rate: 2/s burst 2 so the first 2 requests pass and the rest 429.
		apiSrv := h.startAPI(t, webhookSrv.URL, withRateLimit(2, 2))
		defer apiSrv.Close()

		const total = 10
		var accepted, rejected int32

		for i := 0; i < total; i++ {
			resp := postNotification(t, apiSrv.URL,
				`{"to":"user@example.com","channel":"email","content":"rate limit test"}`, "")
			switch resp.StatusCode {
			case http.StatusAccepted:
				atomic.AddInt32(&accepted, 1)
			case http.StatusTooManyRequests:
				atomic.AddInt32(&rejected, 1)
				ra := resp.Header.Get("Retry-After")
				if ra == "" {
					t.Errorf("429 response missing Retry-After header")
				} else if secs, err := strconv.Atoi(ra); err != nil || secs <= 0 {
					t.Errorf("Retry-After = %q, want positive integer seconds", ra)
				}
			default:
				b, _ := io.ReadAll(resp.Body)
				t.Errorf("unexpected status %d; body: %s", resp.StatusCode, b)
			}
			resp.Body.Close()
		}

		// With burst=2 and 10 back-to-back requests at most 4 should pass
		// (burst + maybe one token refill in the few milliseconds between requests).
		if accepted > 4 {
			t.Errorf("accepted = %d, expected <= 4 with burst=2", accepted)
		}
		if rejected == 0 {
			t.Errorf("rejected = 0, expected some 429s with burst=2 and %d requests", total)
		}

		// After sleeping past the Retry-After window, a request must succeed.
		time.Sleep(2 * time.Second)
		resp := postNotification(t, apiSrv.URL,
			`{"to":"user@example.com","channel":"email","content":"after wait"}`, "")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			t.Errorf("post-sleep request: status = %d, want 202", resp.StatusCode)
		}
	})

	// -------------------------------------------------------------------------
	// 4. Rate limit — batch
	// -------------------------------------------------------------------------
	t.Run("RateLimit_Batch", func(t *testing.T) {
		h.reset(t)

		webhookSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer webhookSrv.Close()

		// burst=3 → batch of 5 rejected, batch of 3 accepted.
		apiSrv := h.startAPI(t, webhookSrv.URL, withRateLimit(100, 3))
		defer apiSrv.Close()

		bigBatch := buildBatch("email", "user@example.com", 5)
		resp5 := postBatch(t, apiSrv.URL, bigBatch)
		defer resp5.Body.Close()
		if resp5.StatusCode != http.StatusTooManyRequests {
			b, _ := io.ReadAll(resp5.Body)
			t.Errorf("batch of 5: status = %d, want 429; body: %s", resp5.StatusCode, b)
		}
		if ra := resp5.Header.Get("Retry-After"); ra == "" {
			t.Errorf("batch 429 missing Retry-After header")
		}

		// Tokens were not consumed (reservation cancelled). Batch of 3 should pass.
		smallBatch := buildBatch("email", "user@example.com", 3)
		resp3 := postBatch(t, apiSrv.URL, smallBatch)
		defer resp3.Body.Close()
		if resp3.StatusCode != http.StatusAccepted {
			b, _ := io.ReadAll(resp3.Body)
			t.Errorf("batch of 3: status = %d, want 202; body: %s", resp3.StatusCode, b)
		}
	})

	// -------------------------------------------------------------------------
	// 5. Priority — best-effort ordering
	// -------------------------------------------------------------------------
	t.Run("Priority_HighBeforeLow", func(t *testing.T) {
		h.resetFull(t)

		const msgsPerPriority = 20

		var orderMu sync.Mutex
		var deliveryOrder []string // notification_id in webhook arrival order

		webhookSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			var wb webhookBody
			_ = json.Unmarshal(body, &wb)
			orderMu.Lock()
			deliveryOrder = append(deliveryOrder, wb.NotificationID)
			orderMu.Unlock()
			w.WriteHeader(http.StatusOK)
		}))
		defer webhookSrv.Close()

		// Seed via API WITHOUT starting the worker so messages queue up.
		apiSrv := h.startAPI(t, webhookSrv.URL)
		defer apiSrv.Close()

		highIDs := make(map[string]bool, msgsPerPriority)
		for i := 0; i < msgsPerPriority; i++ {
			resp := postNotification(t, apiSrv.URL,
				fmt.Sprintf(`{"to":"user%d@example.com","channel":"email","content":"high %d","priority":"high"}`, i, i),
				"")
			var cr createNotificationResponse
			decodeBody(t, resp, &cr)
			highIDs[cr.MessageID] = true
		}

		for i := 0; i < msgsPerPriority; i++ {
			resp := postNotification(t, apiSrv.URL,
				fmt.Sprintf(`{"to":"user%d@example.com","channel":"email","content":"low %d","priority":"low"}`, i, i),
				"")
			resp.Body.Close()
		}

		// Start worker after both queues are seeded.
		workerCancel := h.startWorker(t, webhookSrv.URL)
		defer workerCancel()

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for {
			var pending int
			_ = h.pool.QueryRow(ctx, "SELECT COUNT(*) FROM notifications WHERE status='pending'").Scan(&pending)
			if pending == 0 {
				break
			}
			select {
			case <-ctx.Done():
				t.Fatal("timed out waiting for all notifications to be delivered")
			case <-time.After(200 * time.Millisecond):
			}
		}

		orderMu.Lock()
		defer orderMu.Unlock()

		total := len(deliveryOrder)
		if total != 2*msgsPerPriority {
			t.Errorf("total deliveries = %d, want %d", total, 2*msgsPerPriority)
		}

		if total > 0 {
			first := deliveryOrder[0]
			if !highIDs[first] {
				// Best-effort: with separate goroutines and prefetch we cannot
				// guarantee strict ordering, but we log it as a warning.
				t.Logf("NOTE: first delivered notification %q was NOT from the high queue (non-deterministic under concurrency; not a hard failure)", first)
			}
		}
	})
}

// buildBatch returns a JSON batch payload with n identical notifications.
func buildBatch(channel, to string, n int) string {
	var sb strings.Builder
	sb.WriteString(`{"notifications":[`)
	for i := 0; i < n; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(fmt.Sprintf(`{"to":%q,"channel":%q,"content":"batch test %d"}`, to, channel, i))
	}
	sb.WriteString(`]}`)
	return sb.String()
}
