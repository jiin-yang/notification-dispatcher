//go:build e2e

// Package e2e_test contains end-to-end tests for the notification-dispatcher
// service. They exercise the full path: HTTP API → Postgres → RabbitMQ →
// worker → webhook provider → status update.
//
// Prerequisites: docker compose is running (`make dev-up`).
//
// Run:
//
//	go test -tags=e2e -v -timeout 60s ./test/e2e/...
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
	"github.com/jiin-yang/notification-dispatcher/internal/platform"
)

const (
	// testDBName is the isolated Postgres database used by the e2e suite.
	testDBName = "notifications_test"

	// Production DB connection params reused to connect to the postgres
	// maintenance database for CREATE DATABASE.
	adminDSN = "postgres://notifier:notifier@localhost:5432/postgres?sslmode=disable"
	testDSN  = "postgres://notifier:notifier@localhost:5432/" + testDBName + "?sslmode=disable"

	rmqURL = "amqp://guest:guest@localhost:5672/"

	// Test-scoped RabbitMQ topology names — isolated from the dev topology.
	testExchange   = "test_notifications.topic"
	testQueue      = "test_notifications.default"
	testBindingKey = "#"

)

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

	// Ensure test DB exists.
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

	topo := rabbitmq.Topology{
		Exchange:   testExchange,
		Queue:      testQueue,
		BindingKey: testBindingKey,
	}

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

func (h *harness) close() {
	_ = h.rmqConn.Close()
	h.pool.Close()
}

// reset truncates the notifications table and purges the test queue so each
// subtest starts with no leftover rows or stale messages from previous runs.
// Phase 1 subtests call this; Phase 2 subtests call resetFull which also
// clears the batches table.
func (h *harness) reset(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := h.pool.Exec(ctx, "TRUNCATE TABLE notifications"); err != nil {
		t.Fatalf("truncate notifications: %v", err)
	}
	ch, err := h.rmqConn.Channel()
	if err != nil {
		t.Fatalf("open channel for purge: %v", err)
	}
	defer ch.Close()
	if _, err := ch.QueuePurge(h.topo.Queue, false); err != nil {
		t.Fatalf("purge test queue: %v", err)
	}
}

// resetFull truncates both notifications and batches (which notifications
// references via FK) and purges the queue.
func (h *harness) resetFull(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// TRUNCATE ... CASCADE handles the FK from notifications.batch_id → batches.id.
	if _, err := h.pool.Exec(ctx, "TRUNCATE TABLE batches CASCADE"); err != nil {
		t.Fatalf("truncate batches: %v", err)
	}
	ch, err := h.rmqConn.Channel()
	if err != nil {
		t.Fatalf("open channel for purge: %v", err)
	}
	defer ch.Close()
	if _, err := ch.QueuePurge(h.topo.Queue, false); err != nil {
		t.Fatalf("purge test queue: %v", err)
	}
}

// startAPI wires the API in-process and returns a running httptest.Server.
// The caller owns Close().
func (h *harness) startAPI(t *testing.T, webhookURL string) *httptest.Server {
	t.Helper()

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
		Logger:  log,
		Service: svc,
	})

	srv := httptest.NewServer(router)
	_ = webhookURL // stored in workerDeps; declared here for documentation clarity
	return srv
}

// startWorker registers a consumer on the test queue synchronously (so the
// caller knows messages won't be dropped), then starts the delivery loop in a
// background goroutine. Returns a cancel func; calling it stops the worker.
func (h *harness) startWorker(t *testing.T, webhookURL string) context.CancelFunc {
	t.Helper()

	consumeCh, err := h.rmqConn.Channel()
	if err != nil {
		t.Fatalf("open consumer channel: %v", err)
	}

	if err := consumeCh.Qos(5, 0, false); err != nil {
		_ = consumeCh.Close()
		t.Fatalf("set qos: %v", err)
	}

	consumerTag := fmt.Sprintf("e2e-worker-%d", time.Now().UnixNano())
	ctx, cancel := context.WithCancel(context.Background())

	// Register the subscription synchronously so no messages are lost between
	// startWorker returning and the goroutine scheduling.
	deliveries, err := consumeCh.ConsumeWithContext(
		ctx,
		h.topo.Queue,
		consumerTag,
		false, // autoAck
		false, // exclusive
		false, // noLocal
		false, // noWait
		nil,
	)
	if err != nil {
		cancel()
		_ = consumeCh.Close()
		t.Fatalf("start consume on %s: %v", h.topo.Queue, err)
	}

	webhook := provider.NewWebhookProvider(provider.WebhookOptions{
		URL:     webhookURL,
		Timeout: 5 * time.Second,
	})
	repo := pgadapter.NewNotificationRepository(h.pool)
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	deliver := app.NewDeliverUseCase(webhook, repo, log)

	var wg sync.WaitGroup
	wg.Add(1)

	t.Cleanup(func() {
		cancel()
		wg.Wait()       // wait for the goroutine to drain and exit
		_ = consumeCh.Close()
	})

	go func() {
		defer wg.Done()
		for {
			select {
			case <-ctx.Done():
				// When the context is cancelled, amqp091-go will close the
				// deliveries channel. Drain remaining in-flight messages.
				for d := range deliveries {
					processDelivery(context.Background(), d, deliver)
				}
				return
			case d, ok := <-deliveries:
				if !ok {
					return
				}
				processDelivery(ctx, d, deliver)
			}
		}
	}()

	return cancel
}

func processDelivery(ctx context.Context, d amqp.Delivery, deliver *app.DeliverUseCase) {
	corrID := ""
	if d.Headers != nil {
		if v, ok := d.Headers["correlation_id"].(string); ok {
			corrID = v
		}
	}
	if err := deliver.Handle(ctx, d.Body, corrID); err != nil {
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

// ensureTestDB creates notifications_test if it does not exist. Connects via
// the admin/maintenance "postgres" database.
func ensureTestDB(t *testing.T) {
	t.Helper()
	db, err := sql.Open("pgx", adminDSN)
	if err != nil {
		t.Fatalf("open admin db: %v", err)
	}
	defer db.Close()

	// Check if DB exists.
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
// Using runtime.Caller is the only approach that works regardless of the
// working directory go test is invoked from.
func projectRoot() string {
	_, thisFile, _, _ := runtime.Caller(0)
	// thisFile = .../notification-dispatcher/test/e2e/helpers_test.go
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
