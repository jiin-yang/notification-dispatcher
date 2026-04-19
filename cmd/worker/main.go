package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jiin-yang/notification-dispatcher/internal/adapter/postgres"
	"github.com/jiin-yang/notification-dispatcher/internal/adapter/provider"
	"github.com/jiin-yang/notification-dispatcher/internal/adapter/rabbitmq"
	"github.com/jiin-yang/notification-dispatcher/internal/app"
	"github.com/jiin-yang/notification-dispatcher/internal/config"
	"github.com/jiin-yang/notification-dispatcher/internal/domain"
	"github.com/jiin-yang/notification-dispatcher/internal/platform"
	"github.com/jiin-yang/notification-dispatcher/internal/platform/logger"
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

	deliver := app.NewDeliverUseCase(cbReg, notifRepo, log).
		WithProcessedMarker(processedRepo).
		WithRetryPublisher(retryAdapter).
		WithAttemptRecorder(attemptsRepo)

	hostname, _ := os.Hostname()

	// One consumer goroutine per queue binding, each with its own AMQP channel.
	consumers := make([]app.ConsumerRunner, 0, len(topo.Bindings))
	for i, b := range topo.Bindings {
		consumeCh, err := rmqConn.Channel()
		if err != nil {
			return fmt.Errorf("open consumer channel for %s: %w", b.Queue, err)
		}
		defer func() { _ = consumeCh.Close() }()

		consumer, err := rabbitmq.NewConsumer(consumeCh, b.Queue, rabbitmq.ConsumerOptions{
			ConsumerTag:     fmt.Sprintf("worker-%s-%s-%d", hostname, b.Queue, i),
			PrefetchCount:   10,
			ShutdownTimeout: cfg.ShutdownTimeout,
			Logger:          log,
		})
		if err != nil {
			return fmt.Errorf("create consumer for %s: %w", b.Queue, err)
		}
		consumers = append(consumers, consumer)
		log.Info("consumer registered", "queue", b.Queue)
	}

	log.Info("starting workers", "queue_count", len(consumers))
	return app.RunWorker(rootCtx, app.WorkerDeps{
		Consumers:      consumers,
		DeliverUseCase: deliver,
		Logger:         log,
	})
}
