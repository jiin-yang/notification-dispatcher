//go:build e2e

package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jiin-yang/notification-dispatcher/internal/adapter/provider"
	"github.com/jiin-yang/notification-dispatcher/internal/adapter/rabbitmq"
	"github.com/jiin-yang/notification-dispatcher/internal/domain"
)

// Short TTLs for e2e retry tests so the suite finishes in seconds.
var fastTTLs = rabbitmq.RetryTTLs{
	Level1: 200 * time.Millisecond,
	Level2: 300 * time.Millisecond,
	Level3: 400 * time.Millisecond,
}

// TestPhase4 is the root suite for Phase 4 retry/DLQ/circuit-breaker tests.
func TestPhase4(t *testing.T) {
	h := newHarnessWithTTLs(t, fastTTLs)
	t.Cleanup(h.close)

	// -------------------------------------------------------------------------
	// 1. Retry chain success: 500 twice then 200
	// -------------------------------------------------------------------------
	t.Run("RetryChainSuccess", func(t *testing.T) {
		h.resetFull(t)

		var webhookHits int32
		webhookSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			n := atomic.AddInt32(&webhookHits, 1)
			if n <= 2 {
				w.WriteHeader(http.StatusInternalServerError) // fail first two
			} else {
				w.WriteHeader(http.StatusOK) // succeed on third
			}
		}))
		defer webhookSrv.Close()

		apiSrv := h.startAPI(t, webhookSrv.URL)
		defer apiSrv.Close()
		workerCancel := h.startWorkerPhase4(t, webhookSrv.URL, nil)
		defer workerCancel()

		resp := postNotification(t, apiSrv.URL,
			`{"to":"user@example.com","channel":"email","content":"retry chain test"}`, "")
		if resp.StatusCode != http.StatusAccepted {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			t.Fatalf("POST: status %d; body: %s", resp.StatusCode, b)
		}
		var cr createNotificationResponse
		decodeBody(t, resp, &cr)

		// Wait for delivered (up to 10s with short TTLs).
		status := h.pollStatusAny(t, cr.MessageID, 10*time.Second, "delivered", "failed")
		if status != "delivered" {
			t.Errorf("final status = %q, want delivered", status)
		}

		got := atomic.LoadInt32(&webhookHits)
		if got != 3 {
			t.Errorf("webhook hits = %d, want 3 (2 failures + 1 success)", got)
		}

		// delivery_attempts: attempt 0 → retrying, attempt 1 → retrying, attempt 2 → success
		attempts := h.listAttemptStatuses(t, cr.MessageID)
		if len(attempts) != 3 {
			t.Errorf("delivery_attempts count = %d, want 3; statuses: %v", len(attempts), attempts)
		} else {
			want := []string{"retrying", "retrying", "success"}
			for i, s := range attempts {
				if s != want[i] {
					t.Errorf("attempt[%d] status = %q, want %q", i, s, want[i])
				}
			}
		}
	})

	// -------------------------------------------------------------------------
	// 2. Retry exhausts to DLQ
	// -------------------------------------------------------------------------
	t.Run("RetryExhaustsIntoDLQ", func(t *testing.T) {
		h.resetFull(t)

		var webhookHits int32
		webhookSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			atomic.AddInt32(&webhookHits, 1)
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer webhookSrv.Close()

		apiSrv := h.startAPI(t, webhookSrv.URL)
		defer apiSrv.Close()
		workerCancel := h.startWorkerPhase4(t, webhookSrv.URL, nil)
		defer workerCancel()

		resp := postNotification(t, apiSrv.URL,
			`{"to":"user@example.com","channel":"email","content":"exhaust test"}`, "")
		if resp.StatusCode != http.StatusAccepted {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			t.Fatalf("POST: status %d; body: %s", resp.StatusCode, b)
		}
		var cr createNotificationResponse
		decodeBody(t, resp, &cr)

		// Wait for the notification to become failed (up to 8s with 200+300+400ms TTLs).
		status := h.pollStatusAny(t, cr.MessageID, 8*time.Second, "failed")
		if status != "failed" {
			t.Errorf("final status = %q, want failed", status)
		}

		// Allow brief time for DLQ message to land.
		time.Sleep(300 * time.Millisecond)

		got := atomic.LoadInt32(&webhookHits)
		if got != 4 {
			t.Errorf("webhook hits = %d, want 4 (initial + 3 retries)", got)
		}

		dlqCount := h.dlqMessageCount(t, "email")
		if dlqCount != 1 {
			t.Errorf("DLQ message count = %d, want 1", dlqCount)
		}

		attempts := h.listAttemptStatuses(t, cr.MessageID)
		if len(attempts) != 4 {
			t.Errorf("delivery_attempts count = %d, want 4; statuses: %v", len(attempts), attempts)
		} else {
			// attempts 0,1,2 → retrying; attempt 3 → dlq
			for i := 0; i < 3; i++ {
				if attempts[i] != "retrying" {
					t.Errorf("attempt[%d] = %q, want retrying", i, attempts[i])
				}
			}
			if attempts[3] != "dlq" {
				t.Errorf("attempt[3] = %q, want dlq", attempts[3])
			}
		}
	})

	// -------------------------------------------------------------------------
	// 3. Poison message → DLQ direct (no retry loop)
	// -------------------------------------------------------------------------
	t.Run("PoisonMessageToDLQDirect", func(t *testing.T) {
		h.resetFull(t)

		webhookSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer webhookSrv.Close()

		// Start a worker with retry publisher so it routes to DLQ instead of
		// nacking to the broker.
		workerCancel := h.startWorkerPhase4(t, webhookSrv.URL, nil)
		defer workerCancel()

		// Publish raw garbage bytes to the email.normal queue (via test exchange).
		poisonBody := []byte("this is not valid json {{{{")
		h.publishDirect(t, "email.normal", map[string]any{
			"correlation_id": uuid.NewString(),
		}, poisonBody)

		// Allow time for the worker to process the poison message.
		time.Sleep(500 * time.Millisecond)

		// The poison message must land in the DLQ, not loop through retries.
		dlqCount := h.dlqMessageCount(t, "email")
		if dlqCount != 1 {
			t.Errorf("DLQ email count = %d, want 1 (poison message should go straight to DLQ)", dlqCount)
		}

		// No delivery_attempts rows because we couldn't parse the notification_id.
		var totalAttempts int
		_ = h.pool.QueryRow(context.Background(),
			`SELECT COUNT(*) FROM delivery_attempts`,
		).Scan(&totalAttempts)
		if totalAttempts != 0 {
			t.Logf("delivery_attempts = %d (non-parseable body has no notification_id to record against)", totalAttempts)
		}
	})

	// -------------------------------------------------------------------------
	// 4. Circuit breaker opens after threshold failures
	// -------------------------------------------------------------------------
	t.Run("CircuitBreakerOpens", func(t *testing.T) {
		h.resetFull(t)

		var webhookHits int32
		// Webhook always returns 500 to keep tripping the breaker.
		webhookSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			atomic.AddInt32(&webhookHits, 1)
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer webhookSrv.Close()

		// threshold=2: after 2 consecutive failures the breaker opens.
		// open duration=500ms: longer than our retry TTLs so we can observe it.
		cbConfig := provider.CBConfig{
			FailureThreshold: 2,
			OpenDuration:     500 * time.Millisecond,
			HalfOpenMaxCalls: 1,
		}

		reg := provider.NewRegistry()
		webhook := provider.NewWebhookProvider(provider.WebhookOptions{
			URL:     webhookSrv.URL,
			Timeout: 5 * time.Second,
		})
		reg.MustRegister(domain.ChannelEmail, webhook)
		reg.MustRegister(domain.ChannelSMS, webhook)
		reg.MustRegister(domain.ChannelPush, webhook)

		cbLog := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
		cbReg := provider.NewCircuitBreakerRegistry(reg, cbConfig, cbLog)

		workerCancel := h.startWorkerPhase4(t, webhookSrv.URL, cbReg)
		defer workerCancel()

		apiSrv := h.startAPI(t, webhookSrv.URL)
		defer apiSrv.Close()

		// Send 5 email notifications. The first 2 will hit the webhook; after
		// the 2nd failure the breaker opens and subsequent initial deliveries
		// are short-circuited without calling the webhook.
		const msgCount = 5
		ids := make([]string, msgCount)
		for i := range ids {
			resp := postNotification(t, apiSrv.URL,
				fmt.Sprintf(`{"to":"user%d@example.com","channel":"email","content":"cb test %d"}`, i, i), "")
			var cr createNotificationResponse
			decodeBody(t, resp, &cr)
			ids[i] = cr.MessageID
		}

		// Wait for all 5 notifications to leave pending (they will cycle through
		// retries and eventually reach DLQ with status=failed, or succeed if
		// the breaker re-closes and the webhook is called again).
		// We tolerate longer wait because retries add latency.
		ctx4, cancel4 := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel4()
		for {
			var pending int
			_ = h.pool.QueryRow(ctx4, "SELECT COUNT(*) FROM notifications WHERE status='pending'").Scan(&pending)
			if pending == 0 {
				break
			}
			select {
			case <-ctx4.Done():
				t.Logf("NOTE: some notifications still pending after timeout; circuit breaker state is non-deterministic")
				goto assertCB
			case <-time.After(200 * time.Millisecond):
			}
		}

	assertCB:
		// The key assertion: at least some delivery_attempts must have
		// circuit_open status, proving the breaker fired.
		var circuitOpenCount int
		_ = h.pool.QueryRow(context.Background(),
			`SELECT COUNT(*) FROM delivery_attempts WHERE status = 'circuit_open'`,
		).Scan(&circuitOpenCount)
		if circuitOpenCount == 0 {
			t.Errorf("circuit_open delivery_attempts = 0, want > 0 after breaker opens with threshold=2 and 5 consecutive failures")
		}

		// Webhook should have been called fewer times than the total notifications×4
		// (initial + 3 retries) because the circuit breaker short-circuits some.
		got := atomic.LoadInt32(&webhookHits)
		t.Logf("webhook hits = %d (circuit_open rows = %d)", got, circuitOpenCount)
		if got >= int32(msgCount*4) {
			t.Errorf("webhook hits = %d, want < %d: circuit breaker should have prevented some calls",
				got, msgCount*4)
		}
	})

	// -------------------------------------------------------------------------
	// 5. Admin DLQ inspect
	// -------------------------------------------------------------------------
	t.Run("AdminDLQInspect", func(t *testing.T) {
		h.resetFull(t)

		// Drive a notification into DLQ by always returning 500.
		var webhookHits int32
		webhookSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			atomic.AddInt32(&webhookHits, 1)
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer webhookSrv.Close()

		apiSrv := h.startAPI(t, webhookSrv.URL)
		defer apiSrv.Close()
		workerCancel := h.startWorkerPhase4(t, webhookSrv.URL, nil)
		defer workerCancel()

		resp := postNotification(t, apiSrv.URL,
			`{"to":"user@example.com","channel":"email","content":"admin inspect test"}`, "")
		var cr createNotificationResponse
		decodeBody(t, resp, &cr)

		// Wait for status=failed (message went through all retries into DLQ).
		h.pollStatusAny(t, cr.MessageID, 8*time.Second, "failed")
		time.Sleep(300 * time.Millisecond)

		// GET /admin/dlq/email
		adminResp, err := http.Get(apiSrv.URL + "/admin/dlq/email")
		if err != nil {
			t.Fatalf("GET /admin/dlq/email: %v", err)
		}
		defer adminResp.Body.Close()

		if adminResp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(adminResp.Body)
			t.Fatalf("GET /admin/dlq/email: status %d; body: %s", adminResp.StatusCode, b)
		}

		var inspectResp struct {
			Channel      string `json:"channel"`
			MessageCount int    `json:"messageCount"`
		}
		if err := json.NewDecoder(adminResp.Body).Decode(&inspectResp); err != nil {
			t.Fatalf("decode inspect response: %v", err)
		}

		if inspectResp.Channel != "email" {
			t.Errorf("channel = %q, want email", inspectResp.Channel)
		}
		if inspectResp.MessageCount != 1 {
			t.Errorf("messageCount = %d, want 1", inspectResp.MessageCount)
		}
	})

	// -------------------------------------------------------------------------
	// 6. Admin DLQ replay
	// -------------------------------------------------------------------------
	t.Run("AdminDLQReplay", func(t *testing.T) {
		h.resetFull(t)

		// Phase 1: webhook returns 500 → notification goes into DLQ.
		var deliverOK int32 // flip to 1 when webhook should start succeeding
		var webhookHits int32
		webhookSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			atomic.AddInt32(&webhookHits, 1)
			if atomic.LoadInt32(&deliverOK) == 1 {
				w.WriteHeader(http.StatusOK)
			} else {
				w.WriteHeader(http.StatusInternalServerError)
			}
		}))
		defer webhookSrv.Close()

		apiSrv := h.startAPI(t, webhookSrv.URL)
		defer apiSrv.Close()
		workerCancel := h.startWorkerPhase4(t, webhookSrv.URL, nil)
		defer workerCancel()

		resp := postNotification(t, apiSrv.URL,
			`{"to":"user@example.com","channel":"email","content":"replay test"}`, "")
		var cr createNotificationResponse
		decodeBody(t, resp, &cr)

		// Wait for notification to reach failed (in DLQ).
		h.pollStatusAny(t, cr.MessageID, 8*time.Second, "failed")
		time.Sleep(300 * time.Millisecond)

		// Verify it's in the DLQ.
		dlqBefore := h.dlqMessageCount(t, "email")
		if dlqBefore != 1 {
			t.Fatalf("DLQ count before replay = %d, want 1", dlqBefore)
		}

		// Phase 2: flip webhook to succeed, replay from DLQ.
		atomic.StoreInt32(&deliverOK, 1)

		replayReq, err := http.NewRequest(http.MethodPost,
			apiSrv.URL+"/admin/dlq/email/replay?limit=10", nil)
		if err != nil {
			t.Fatalf("build replay request: %v", err)
		}
		replayResp, err := http.DefaultClient.Do(replayReq)
		if err != nil {
			t.Fatalf("POST /admin/dlq/email/replay: %v", err)
		}
		defer replayResp.Body.Close()

		if replayResp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(replayResp.Body)
			t.Fatalf("replay status %d; body: %s", replayResp.StatusCode, b)
		}

		var replayBody struct {
			Replayed  int `json:"replayed"`
			Remaining int `json:"remaining"`
		}
		if err := json.NewDecoder(replayResp.Body).Decode(&replayBody); err != nil {
			t.Fatalf("decode replay response: %v", err)
		}

		if replayBody.Replayed != 1 {
			t.Errorf("replayed = %d, want 1", replayBody.Replayed)
		}
		if replayBody.Remaining != 0 {
			t.Errorf("remaining = %d, want 0", replayBody.Remaining)
		}

		// The replayed message re-enters with attempt=0 and should be delivered.
		// Poll until delivered (worker picks it up from the main queue).
		status := h.pollStatusAny(t, cr.MessageID, 10*time.Second, "delivered")
		if status != "delivered" {
			t.Errorf("post-replay status = %q, want delivered", status)
		}

		// DLQ must now be empty.
		dlqAfter := h.dlqMessageCount(t, "email")
		if dlqAfter != 0 {
			t.Errorf("DLQ count after replay = %d, want 0", dlqAfter)
		}
	})
}

// nopLogger returns a slog.Logger that discards all output.
func nopLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 100}))
}

// ---- helpers used only in phase4 tests --------------------------------------

// buildBatchPhase4 returns a JSON batch payload with n identical email notifications.
func buildBatchPhase4(n int) string {
	var sb strings.Builder
	sb.WriteString(`{"notifications":[`)
	for i := 0; i < n; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(fmt.Sprintf(`{"to":"user%d@example.com","channel":"email","content":"phase4 batch %d"}`, i, i))
	}
	sb.WriteString(`]}`)
	return sb.String()
}
