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

	topoCh, err := rmqConn.Channel()
	if err != nil {
		return fmt.Errorf("open topology channel: %w", err)
	}
	if err := rabbitmq.DeclareTopology(topoCh); err != nil {
		return fmt.Errorf("declare topology: %w", err)
	}
	_ = topoCh.Close()

	consumeCh, err := rmqConn.Channel()
	if err != nil {
		return fmt.Errorf("open consumer channel: %w", err)
	}
	defer func() { _ = consumeCh.Close() }()

	webhook := provider.NewWebhookProvider(provider.WebhookOptions{
		URL:     cfg.WebhookURL,
		Timeout: cfg.WebhookTimeout,
	})

	repo := postgres.NewNotificationRepository(pool)
	deliver := app.NewDeliverUseCase(webhook, repo, log)

	consumer, err := rabbitmq.NewConsumer(consumeCh, rabbitmq.QueueDefault, rabbitmq.ConsumerOptions{
		ConsumerTag:     "worker-" + os.Getenv("HOSTNAME"),
		PrefetchCount:   10,
		ShutdownTimeout: cfg.ShutdownTimeout,
		Logger:          log,
	})
	if err != nil {
		return fmt.Errorf("create consumer: %w", err)
	}

	log.Info("consuming", "queue", rabbitmq.QueueDefault)
	return app.RunWorker(rootCtx, app.WorkerDeps{
		Consumer:       consumer,
		DeliverUseCase: deliver,
		Logger:         log,
	})
}
