//go:build e2e

// Package e2e_test contains end-to-end tests for the notification-dispatcher
// service. They exercise the full path: HTTP API → Postgres → RabbitMQ →
// worker → webhook provider → status update.
//
// Prerequisites: docker compose is running (`make dev-up`).
//
// Run:
//
//	go test -tags=e2e -v -timeout 120s ./test/e2e/...
//
// The suite creates and migrates a dedicated `notifications_test` database and
// uses a `test_notifications.*` RabbitMQ topology so it never conflicts with
// a concurrently-running dev environment.
package e2e_test

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/jackc/pgx/v5/pgxpool"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/pressly/goose/v3"

	httpadapter "github.com/jiin-yang/notification-dispatcher/internal/adapter/http"
	pgadapter "github.com/jiin-yang/notification-dispatcher/internal/adapter/postgres"
	"github.com/jiin-yang/notification-dispatcher/internal/adapter/provider"
	"github.com/jiin-yang/notification-dispatcher/internal/adapter/rabbitmq"
	"github.com/jiin-yang/notification-dispatcher/internal/app"
	"github.com/jiin-yang/notification-dispatcher/internal/domain"
	"github.com/jiin-yang/notification-dispatcher/internal/platform"
	"github.com/jiin-yang/notification-dispatcher/internal/platform/ratelimit"
)

const (
	testDBName = "notifications_test"

	adminDSN = "postgres://notifier:notifier@localhost:5432/postgres?sslmode=disable"
	testDSN  = "postgres://notifier:notifier@localhost:5432/" + testDBName + "?sslmode=disable"

	rmqURL = "amqp://guest:guest@localhost:5672/"

	// Test-scoped RabbitMQ exchange name — isolated from the dev exchange.
	testExchange = "test_notifications.topic"
)

// testBindings returns a set of test-prefixed bindings mirroring production.
// Tests use a "test_" prefix so they never conflict with a running dev worker.
func testBindings() []rabbitmq.QueueBinding {
	bindings := rabbitmq.AllProductionBindings()
	for i := range bindings {
		bindings[i].Queue = "test_" + bindings[i].Queue
		// RoutingKey stays the same so the publisher's routing key still matches.
	}
	return bindings
}

// testTopology returns the full 9-queue test topology (no retry/DLQ queues).
// Phase 1/2/3 tests use this.
func testTopology() rabbitmq.Topology {
	return rabbitmq.Topology{
		Exchange:  testExchange,
		Bindings:  testBindings(),
		Prefix:    "test_",
		RetryTTLs: rabbitmq.ProductionRetryTTLs(),
	}
}

// phase4ExchangeName is the test exchange for Phase 4 tests. Using a separate
// exchange (and "p4_" prefixed queues) avoids declaration conflicts with the
// Phase 1-3 test topology which uses different TTLs.
const phase4ExchangeName = "test_p4_notifications.topic"

// phase4TestBindings returns test bindings with "test_p4_" prefix.
func phase4TestBindings() []rabbitmq.QueueBinding {
	bindings := rabbitmq.AllProductionBindings()
	for i := range bindings {
		bindings[i].Queue = "test_p4_" + bindings[i].Queue
	}
	return bindings
}

// phase4TestTopology returns a topology with short TTLs suitable for fast e2e
// retry tests. Uses "test_p4_" prefix to avoid conflict with the base topology.
func phase4TestTopology(ttls rabbitmq.RetryTTLs) rabbitmq.Topology {
	return rabbitmq.Topology{
		Exchange:  phase4ExchangeName,
		Bindings:  phase4TestBindings(),
		Prefix:    "test_p4_",
		RetryTTLs: ttls,
	}
}

// harness holds the shared infrastructure for the test suite.
type harness struct {
	pool    *pgxpool.Pool
	rmqConn *rabbitmq.Connection
	topo    rabbitmq.Topology
}

// newHarness sets up the test DB (create + migrate) and connects to RabbitMQ.
// Call h.close() in a defer to release resources.
func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()

	ensureTestDB(t)

	pool, err := platform.NewPgxPool(ctx, testDSN)
	if err != nil {
		t.Fatalf("connect test postgres: %v", err)
	}

	runMigrations(t, testDSN)

	rmqConn, err := rabbitmq.Dial(rmqURL)
	if err != nil {
		pool.Close()
		t.Fatalf("connect rabbitmq: %v", err)
	}

	topo := testTopology()

	topoCh, err := rmqConn.Channel()
	if err != nil {
		_ = rmqConn.Close()
		pool.Close()
		t.Fatalf("open topology channel: %v", err)
	}
	if err := rabbitmq.DeclareTopologyWith(topoCh, topo); err != nil {
		_ = topoCh.Close()
		_ = rmqConn.Close()
		pool.Close()
		t.Fatalf("declare test topology: %v", err)
	}
	_ = topoCh.Close()

	return &harness{pool: pool, rmqConn: rmqConn, topo: topo}
}

// newHarnessWithTTLs creates a harness that also declares retry/DLQ topology
// with the given TTLs. Used by Phase 4 tests.
func newHarnessWithTTLs(t *testing.T, ttls rabbitmq.RetryTTLs) *harness {
	t.Helper()
	ctx := context.Background()

	ensureTestDB(t)

	pool, err := platform.NewPgxPool(ctx, testDSN)
	if err != nil {
		t.Fatalf("connect test postgres: %v", err)
	}

	runMigrations(t, testDSN)

	rmqConn, err := rabbitmq.Dial(rmqURL)
	if err != nil {
		pool.Close()
		t.Fatalf("connect rabbitmq: %v", err)
	}

	topo := phase4TestTopology(ttls)

	topoCh, err := rmqConn.Channel()
	if err != nil {
		_ = rmqConn.Close()
		pool.Close()
		t.Fatalf("open topology channel: %v", err)
	}
	if err := rabbitmq.DeclareTopologyWith(topoCh, topo); err != nil {
		_ = topoCh.Close()
		_ = rmqConn.Close()
		pool.Close()
		t.Fatalf("declare phase4 test topology: %v", err)
	}
	_ = topoCh.Close()

	return &harness{pool: pool, rmqConn: rmqConn, topo: topo}
}

func (h *harness) close() {
	_ = h.rmqConn.Close()
	h.pool.Close()
}

// reset truncates the notifications, processed_messages, and delivery_attempts
// tables and purges all test queues so each subtest starts clean.
// CASCADE ensures any FK-referencing tables (delivery_attempts) are cleared too.
func (h *harness) reset(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := h.pool.Exec(ctx, "TRUNCATE TABLE notifications CASCADE"); err != nil {
		t.Fatalf("truncate notifications: %v", err)
	}
	if _, err := h.pool.Exec(ctx, "TRUNCATE TABLE processed_messages"); err != nil {
		t.Fatalf("truncate processed_messages: %v", err)
	}

	h.purgeAllQueues(t)
}

// resetFull truncates batches (cascades to notifications and delivery_attempts),
// processed_messages, and purges all test queues.
func (h *harness) resetFull(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := h.pool.Exec(ctx, "TRUNCATE TABLE batches CASCADE"); err != nil {
		t.Fatalf("truncate batches: %v", err)
	}
	if _, err := h.pool.Exec(ctx, "TRUNCATE TABLE notifications CASCADE"); err != nil {
		t.Fatalf("truncate notifications: %v", err)
	}
	if _, err := h.pool.Exec(ctx, "TRUNCATE TABLE processed_messages"); err != nil {
		t.Fatalf("truncate processed_messages: %v", err)
	}

	h.purgeAllQueues(t)
}

func (h *harness) purgeAllQueues(t *testing.T) {
	t.Helper()
	ch, err := h.rmqConn.Channel()
	if err != nil {
		t.Fatalf("open channel for purge: %v", err)
	}
	defer ch.Close()

	// Purge main queues.
	for _, b := range h.topo.Bindings {
		if _, err := ch.QueuePurge(b.Queue, false); err != nil {
			t.Logf("purge %s: %v (ok if queue doesn't exist yet)", b.Queue, err)
		}
	}

	// Purge retry and DLQ queues (may not exist yet in non-phase4 tests — that's ok).
	for _, channel := range []string{"email", "sms", "push"} {
		for _, priority := range []string{"high", "normal", "low"} {
			for level := 1; level <= 3; level++ {
				qName := h.topo.RetryQueueName(channel, priority, level)
				if _, err := ch.QueuePurge(qName, false); err != nil {
					t.Logf("purge retry %s: %v (ok if queue doesn't exist)", qName, err)
				}
			}
		}
		dlqName := h.topo.DLQQueueName(channel)
		if _, err := ch.QueuePurge(dlqName, false); err != nil {
			t.Logf("purge dlq %s: %v (ok if queue doesn't exist)", dlqName, err)
		}
	}
}

// startAPI wires the API in-process and returns a running httptest.Server.
// opts may be provided to override default wiring (e.g. inject a custom rate
// limiter for rate-limit tests). The caller owns Close().
func (h *harness) startAPI(t *testing.T, webhookURL string, opts ...apiOption) *httptest.Server {
	t.Helper()

	cfg := apiConfig{} // defaults: no rate limiter
	for _, o := range opts {
		o(&cfg)
	}

	pubCh, err := h.rmqConn.Channel()
	if err != nil {
		t.Fatalf("open publisher channel: %v", err)
	}
	t.Cleanup(func() { _ = pubCh.Close() })

	publisher, err := rabbitmq.NewPublisher(pubCh, h.topo.Exchange)
	if err != nil {
		t.Fatalf("create publisher: %v", err)
	}

	repo := pgadapter.NewNotificationRepository(h.pool)
	svc := app.NewNotificationService(repo, publisher)

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	router := httpadapter.NewRouter(httpadapter.RouterDeps{
		Logger:      log,
		Service:     svc,
		RateLimiter: cfg.rateLimiter,
		// Wire admin routes so Phase 4 tests can use /admin/dlq/*.
		// Use topology prefix/exchange so admin routes target test-scoped queues.
		AMQPChannelProvider: h.rmqConn,
		AdminPublisher:      publisher,
		AdminQueuePrefix:    h.topo.Prefix,
		AdminMainExchange:   h.topo.Exchange,
	})

	srv := httptest.NewServer(router)
	_ = webhookURL
	return srv
}

type apiConfig struct {
	rateLimiter httpadapter.ChannelRateLimiter
}

type apiOption func(*apiConfig)

// withRateLimit injects a custom ChannelLimiter into the API.
func withRateLimit(rps float64, burst int) apiOption {
	return func(cfg *apiConfig) {
		cfg.rateLimiter = ratelimit.New(rps, burst)
	}
}

// startWorker registers consumers on all test queues and starts the delivery
// loop in a background goroutine. Returns a cancel func; calling it stops the
// worker and waits for it to drain.
//
// startWorker uses Phase 1/2/3 compatible behaviour: no retry publisher is
// wired, so 500 webhooks immediately mark the notification failed. Phase 4
// tests use startWorkerPhase4 instead.
func (h *harness) startWorker(t *testing.T, webhookURL string) context.CancelFunc {
	t.Helper()
	return h.startWorkerWithDeliver(t, webhookURL, nil)
}

// startWorkerPhase4 builds a DeliverUseCase with retry publisher and attempt
// recorder wired, then starts the worker. Use in Phase 4 subtests where you
// need retry/DLQ routing and delivery_attempts recording.
func (h *harness) startWorkerPhase4(t *testing.T, webhookURL string, providerOverride domain.Provider) context.CancelFunc {
	t.Helper()

	var prov domain.Provider
	if providerOverride != nil {
		prov = providerOverride
	} else {
		prov = provider.NewWebhookProvider(provider.WebhookOptions{
			URL:     webhookURL,
			Timeout: 5 * time.Second,
		})
	}

	repo := pgadapter.NewNotificationRepository(h.pool)
	processedRepo := pgadapter.NewProcessedRepository(h.pool)
	attemptsRepo := pgadapter.NewDeliveryAttemptsRepository(h.pool)
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	retryPubCh, err := h.rmqConn.Channel()
	if err != nil {
		t.Fatalf("open retry publisher channel: %v", err)
	}
	t.Cleanup(func() { _ = retryPubCh.Close() })

	retryPub, err := rabbitmq.NewPublisher(retryPubCh, h.topo.Exchange)
	if err != nil {
		t.Fatalf("create retry publisher: %v", err)
	}
	retryAdapter := rabbitmq.NewRetryPublisherAdapter(retryPub, h.topo)

	deliver := app.NewDeliverUseCase(prov, repo, log).
		WithProcessedMarker(processedRepo).
		WithRetryPublisher(retryAdapter).
		WithAttemptRecorder(attemptsRepo)

	return h.startWorkerWithDeliver(t, webhookURL, deliver)
}

// startWorkerWithDeliver is like startWorker but accepts a pre-built
// DeliverUseCase. Pass nil to build one from webhookURL WITHOUT retry
// publisher (Phase 1/2/3 compat).
func (h *harness) startWorkerWithDeliver(t *testing.T, webhookURL string, deliver *app.DeliverUseCase) context.CancelFunc {
	t.Helper()

	if deliver == nil {
		webhook := provider.NewWebhookProvider(provider.WebhookOptions{
			URL:     webhookURL,
			Timeout: 5 * time.Second,
		})
		reg := provider.NewRegistry()
		reg.MustRegister(domain.ChannelEmail, webhook)
		reg.MustRegister(domain.ChannelSMS, webhook)
		reg.MustRegister(domain.ChannelPush, webhook)

		repo := pgadapter.NewNotificationRepository(h.pool)
		processedRepo := pgadapter.NewProcessedRepository(h.pool)
		log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

		// No retry publisher for Phase 1/2/3 compat: 500 → immediate failed.
		deliver = app.NewDeliverUseCase(reg, repo, log).
			WithProcessedMarker(processedRepo)
	}

	ctx, cancel := context.WithCancel(context.Background())

	type queueDeliveries struct {
		queue      string
		deliveries <-chan amqp.Delivery
		consumeCh  *amqp.Channel
	}

	qds := make([]queueDeliveries, 0, len(h.topo.Bindings))
	for _, b := range h.topo.Bindings {
		consumeCh, err := h.rmqConn.Channel()
		if err != nil {
			cancel()
			t.Fatalf("open consumer channel for %s: %v", b.Queue, err)
		}

		if err := consumeCh.Qos(5, 0, false); err != nil {
			_ = consumeCh.Close()
			cancel()
			t.Fatalf("set qos on %s: %v", b.Queue, err)
		}

		tag := fmt.Sprintf("e2e-worker-%s-%d", b.Queue, time.Now().UnixNano())
		deliveries, err := consumeCh.ConsumeWithContext(
			ctx,
			b.Queue,
			tag,
			false, // autoAck
			false, // exclusive
			false, // noLocal
			false, // noWait
			nil,
		)
		if err != nil {
			_ = consumeCh.Close()
			cancel()
			t.Fatalf("start consume on %s: %v", b.Queue, err)
		}

		qds = append(qds, queueDeliveries{queue: b.Queue, deliveries: deliveries, consumeCh: consumeCh})
	}

	var wg sync.WaitGroup

	t.Cleanup(func() {
		cancel()
		wg.Wait()
		for _, qd := range qds {
			_ = qd.consumeCh.Close()
		}
	})

	for _, qd := range qds {
		qd := qd
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					for d := range qd.deliveries {
						processDelivery(context.Background(), d, deliver)
					}
					return
				case d, ok := <-qd.deliveries:
					if !ok {
						return
					}
					processDelivery(ctx, d, deliver)
				}
			}
		}()
	}

	return cancel
}

func processDelivery(ctx context.Context, d amqp.Delivery, deliver *app.DeliverUseCase) {
	corrID := ""
	if d.Headers != nil {
		if v, ok := d.Headers["correlation_id"].(string); ok {
			corrID = v
		}
	}
	attempt := int32(0)
	if d.Headers != nil {
		switch v := d.Headers["x-attempt"].(type) {
		case int32:
			attempt = v
		case int64:
			attempt = int32(v)
		case int:
			attempt = int32(v)
		}
	}
	if err := deliver.Handle(ctx, d.Body, corrID, int(attempt), d.RoutingKey); err != nil {
		_ = d.Nack(false, false)
		return
	}
	_ = d.Ack(false)
}

// pollStatus polls the DB until the notification reaches a non-pending status
// or the deadline is exceeded.
func (h *harness) pollStatus(t *testing.T, id string, deadline time.Duration) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()

	for {
		var status string
		err := h.pool.QueryRow(ctx,
			"SELECT status FROM notifications WHERE id = $1", id,
		).Scan(&status)
		if err == nil && status != "pending" {
			return status
		}
		select {
		case <-ctx.Done():
			t.Fatalf("pollStatus: timed out after %s waiting for notification %s to leave pending", deadline, id)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// pollStatusAny polls until the status is one of the given values.
func (h *harness) pollStatusAny(t *testing.T, id string, deadline time.Duration, statuses ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()

	statusSet := make(map[string]bool, len(statuses))
	for _, s := range statuses {
		statusSet[s] = true
	}

	for {
		var status string
		err := h.pool.QueryRow(ctx,
			"SELECT status FROM notifications WHERE id = $1", id,
		).Scan(&status)
		if err == nil && statusSet[status] {
			return status
		}
		select {
		case <-ctx.Done():
			var got string
			_ = h.pool.QueryRow(context.Background(),
				"SELECT status FROM notifications WHERE id = $1", id,
			).Scan(&got)
			t.Fatalf("pollStatusAny: timed out after %s waiting for %v; got %q", deadline, statuses, got)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// publishDirect publishes a raw JSON body directly to the test exchange with
// the given routing key and headers, bypassing the API. Used by idempotency
// tests that need to inject duplicate messages, and by Phase 4 poison tests.
func (h *harness) publishDirect(t *testing.T, routingKey string, headers map[string]any, body []byte) {
	t.Helper()

	pubCh, err := h.rmqConn.Channel()
	if err != nil {
		t.Fatalf("publishDirect: open channel: %v", err)
	}
	defer pubCh.Close()

	pub, err := rabbitmq.NewPublisher(pubCh, h.topo.Exchange)
	if err != nil {
		t.Fatalf("publishDirect: new publisher: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := pub.Publish(ctx, routingKey, headers, body); err != nil {
		t.Fatalf("publishDirect: publish: %v", err)
	}
}

// dlqMessageCount returns the number of messages in a DLQ queue via
// QueueInspect. The queue must be using the topology prefix.
func (h *harness) dlqMessageCount(t *testing.T, channel string) int {
	t.Helper()
	ch, err := h.rmqConn.Channel()
	if err != nil {
		t.Fatalf("dlqMessageCount: open channel: %v", err)
	}
	defer ch.Close()

	qName := h.topo.DLQQueueName(channel)
	q, err := ch.QueueInspect(qName)
	if err != nil {
		t.Fatalf("dlqMessageCount: inspect %s: %v", qName, err)
	}
	return q.Messages
}

// countDeliveryAttempts returns the number of delivery_attempts rows for a
// notification.
func (h *harness) countDeliveryAttempts(t *testing.T, notificationID string) int {
	t.Helper()
	var count int
	err := h.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM delivery_attempts WHERE notification_id = $1`,
		notificationID,
	).Scan(&count)
	if err != nil {
		t.Fatalf("countDeliveryAttempts: %v", err)
	}
	return count
}

// listAttemptStatuses returns the status column of all delivery_attempts for a
// notification ordered by attempt_number, id.
func (h *harness) listAttemptStatuses(t *testing.T, notificationID string) []string {
	t.Helper()
	rows, err := h.pool.Query(context.Background(),
		`SELECT status FROM delivery_attempts
		 WHERE notification_id = $1
		 ORDER BY attempt_number ASC, id ASC`,
		notificationID,
	)
	if err != nil {
		t.Fatalf("listAttemptStatuses: %v", err)
	}
	defer rows.Close()

	var statuses []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan attempt status: %v", err)
		}
		statuses = append(statuses, s)
	}
	return statuses
}

// ensureTestDB creates notifications_test if it does not exist.
func ensureTestDB(t *testing.T) {
	t.Helper()
	db, err := sql.Open("pgx", adminDSN)
	if err != nil {
		t.Fatalf("open admin db: %v", err)
	}
	defer db.Close()

	var exists bool
	err = db.QueryRow(
		"SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname = $1)", testDBName,
	).Scan(&exists)
	if err != nil {
		t.Fatalf("check db existence: %v", err)
	}
	if exists {
		return
	}
	if _, err := db.Exec("CREATE DATABASE " + testDBName); err != nil {
		t.Fatalf("create test db: %v", err)
	}
}

// projectRoot returns the absolute path of the module root by walking two
// directories up from this source file (test/e2e/ → test/ → root/).
func projectRoot() string {
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(thisFile), "..", "..")
}

// runMigrations applies all pending goose migrations to the test DB.
func runMigrations(t *testing.T, dsn string) {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open test db for migrations: %v", err)
	}
	defer db.Close()

	dir := filepath.Join(projectRoot(), "migrations")
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("goose set dialect: %v", err)
	}
	if err := goose.Up(db, dir); err != nil {
		t.Fatalf("goose up: %v", err)
	}
}

