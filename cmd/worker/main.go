package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sony/gobreaker/v2"

	httpadapter "github.com/jiin-yang/notification-dispatcher/internal/adapter/http"
	"github.com/jiin-yang/notification-dispatcher/internal/adapter/postgres"
	"github.com/jiin-yang/notification-dispatcher/internal/adapter/provider"
	"github.com/jiin-yang/notification-dispatcher/internal/adapter/rabbitmq"
	"github.com/jiin-yang/notification-dispatcher/internal/app"
	"github.com/jiin-yang/notification-dispatcher/internal/config"
	"github.com/jiin-yang/notification-dispatcher/internal/domain"
	"github.com/jiin-yang/notification-dispatcher/internal/platform"
	"github.com/jiin-yang/notification-dispatcher/internal/platform/logger"
	"github.com/jiin-yang/notification-dispatcher/internal/platform/metrics"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "worker exited: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := logger.New(cfg.LogLevel).With("service", "worker")

	rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	startCtx, cancelStart := context.WithTimeout(rootCtx, 15*time.Second)
	defer cancelStart()

	pool, err := platform.NewPgxPool(startCtx, cfg.DBDSN)
	if err != nil {
		return fmt.Errorf("connect postgres: %w", err)
	}
	defer pool.Close()
	log.Info("postgres connected")

	rmqConn, err := rabbitmq.Dial(cfg.RMQURL)
	if err != nil {
		return fmt.Errorf("connect rabbitmq: %w", err)
	}
	defer func() { _ = rmqConn.Close() }()
	log.Info("rabbitmq connected")

	topo := rabbitmq.ProductionTopology()

	topoCh, err := rmqConn.Channel()
	if err != nil {
		return fmt.Errorf("open topology channel: %w", err)
	}
	if err := rabbitmq.DeclareTopologyWith(topoCh, topo); err != nil {
		return fmt.Errorf("declare topology: %w", err)
	}
	_ = topoCh.Close()

	// Build the provider registry with circuit breakers.
	webhook := provider.NewWebhookProvider(provider.WebhookOptions{
		URL:     cfg.WebhookURL,
		Timeout: cfg.WebhookTimeout,
	})
	reg := provider.NewRegistry()
	reg.MustRegister(domain.ChannelEmail, webhook)
	reg.MustRegister(domain.ChannelSMS, webhook)
	reg.MustRegister(domain.ChannelPush, webhook)

	cbCfg := provider.CBConfig{
		FailureThreshold: cfg.CBFailureThreshold,
		OpenDuration:     cfg.CBOpenDuration,
		HalfOpenMaxCalls: cfg.CBHalfOpenMaxCalls,
	}
	cbReg := provider.NewCircuitBreakerRegistry(reg, cbCfg, log)

	notifRepo := postgres.NewNotificationRepository(pool)
	processedRepo := postgres.NewProcessedRepository(pool)
	attemptsRepo := postgres.NewDeliveryAttemptsRepository(pool)

	// Dedicated publisher channel for retry/DLQ routing.
	retryPubCh, err := rmqConn.Channel()
	if err != nil {
		return fmt.Errorf("open retry publisher channel: %w", err)
	}
	defer func() { _ = retryPubCh.Close() }()

	retryPub, err := rabbitmq.NewPublisher(retryPubCh, rabbitmq.ExchangeNotifications)
	if err != nil {
		return fmt.Errorf("create retry publisher: %w", err)
	}
	retryAdapter := rabbitmq.NewRetryPublisherAdapter(retryPub, topo)

	m := metrics.New("worker")

	deliver := app.NewDeliverUseCase(cbReg, notifRepo, log).
		WithProcessedMarker(processedRepo).
		WithRetryPublisher(retryAdapter).
		WithAttemptRecorder(attemptsRepo).
		WithMetrics(m)

	hostname, _ := os.Hostname()

	// One consumer goroutine per queue binding, each with its own AMQP channel.
	consumers := make([]app.ConsumerRunner, 0, len(topo.Bindings))
	for i, b := range topo.Bindings {
		consumeCh, err := rmqConn.Channel()
		if err != nil {
			return fmt.Errorf("open consumer channel for %s: %w", b.Queue, err)
		}
		defer func() { _ = consumeCh.Close() }()

		c := concurrencyFor(b.Queue, cfg)
		prefetch := 10
		if c > prefetch {
			prefetch = c
		}
		consumer, err := rabbitmq.NewConsumer(consumeCh, b.Queue, rabbitmq.ConsumerOptions{
			ConsumerTag:     fmt.Sprintf("worker-%s-%s-%d", hostname, b.Queue, i),
			PrefetchCount:   prefetch,
			Concurrency:     c,
			ShutdownTimeout: cfg.ShutdownTimeout,
			Logger:          log,
		})
		if err != nil {
			return fmt.Errorf("create consumer for %s: %w", b.Queue, err)
		}
		consumers = append(consumers, consumer)
		log.Info("consumer registered", "queue", b.Queue, "concurrency", c)
	}

	// Serve /metrics and /health/live from the worker process so scrapers and
	// k8s probes can target it without reusing the api port.
	metricsServer := newWorkerMetricsServer(cfg.MetricsPort, m)
	serverErr := make(chan error, 1)
	go func() {
		log.Info("worker metrics listening", "port", cfg.MetricsPort)
		if err := metricsServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
		close(serverErr)
	}()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		_ = metricsServer.Shutdown(shutdownCtx)
	}()

	// Periodically publish circuit-breaker state as a Prometheus gauge so the
	// worker's /metrics endpoint reflects open breakers without instrumenting
	// the hot path on every delivery.
	go reportBreakerStates(rootCtx, cbReg, m, 5*time.Second)

	log.Info("starting workers", "queue_count", len(consumers))
	return app.RunWorker(rootCtx, app.WorkerDeps{
		Consumers:      consumers,
		DeliverUseCase: deliver,
		Logger:         log,
		Metrics:        m,
	})
}

func newWorkerMetricsServer(port string, m *metrics.Metrics) *http.Server {
	r := chi.NewRouter()
	r.Get("/health/live", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	// Reuse the same docs+metrics helpers as the api for consistency.
	httpadapter.RegisterDocs(r)
	r.Method(http.MethodGet, "/metrics", promhttp.HandlerFor(m.Registry(), promhttp.HandlerOpts{
		ErrorHandling: promhttp.ContinueOnError,
	}))

	return &http.Server{
		Addr:              ":" + port,
		Handler:           r,
		ReadHeaderTimeout: 5 * time.Second,
	}
}

func reportBreakerStates(ctx context.Context, cbReg *provider.CircuitBreakerRegistry, m *metrics.Metrics, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			for _, ch := range cbReg.Channels() {
				v := breakerStateValue(cbReg.BreakerState(ch))
				m.CircuitBreakerState.WithLabelValues(string(ch)).Set(v)
			}
		}
	}
}

// concurrencyFor maps a queue name to its configured concurrency. The queue
// name prefix (email/sms/push) selects which env-based value to use.
func concurrencyFor(queue string, cfg config.Config) int {
	switch {
	case strings.HasPrefix(queue, "email"):
		return cfg.EmailConcurrency
	case strings.HasPrefix(queue, "sms"):
		return cfg.SMSConcurrency
	case strings.HasPrefix(queue, "push"):
		return cfg.PushConcurrency
	default:
		return 1
	}
}

func breakerStateValue(s gobreaker.State) float64 {
	switch s {
	case gobreaker.StateClosed:
		return 0
	case gobreaker.StateHalfOpen:
		return 1
	case gobreaker.StateOpen:
		return 2
	}
	return 0
}
