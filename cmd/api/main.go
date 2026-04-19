package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	httpadapter "github.com/jiin-yang/notification-dispatcher/internal/adapter/http"
	pgadapter "github.com/jiin-yang/notification-dispatcher/internal/adapter/postgres"
	"github.com/jiin-yang/notification-dispatcher/internal/adapter/rabbitmq"
	"github.com/jiin-yang/notification-dispatcher/internal/app"
	"github.com/jiin-yang/notification-dispatcher/internal/config"
	"github.com/jiin-yang/notification-dispatcher/internal/platform"
	"github.com/jiin-yang/notification-dispatcher/internal/platform/logger"
	"github.com/jiin-yang/notification-dispatcher/internal/platform/ratelimit"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "api exited: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := logger.New(cfg.LogLevel).With("service", "api")

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

	pubCh, err := rmqConn.Channel()
	if err != nil {
		return fmt.Errorf("open publisher channel: %w", err)
	}
	defer func() { _ = pubCh.Close() }()

	publisher, err := rabbitmq.NewPublisher(pubCh, rabbitmq.ExchangeNotifications)
	if err != nil {
		return fmt.Errorf("create publisher: %w", err)
	}

	repo := pgadapter.NewNotificationRepository(pool)
	svc := app.NewNotificationService(repo, publisher)

	limiter := ratelimit.New(cfg.RateLimitPerSecond, cfg.RateLimitBurst)

	router := httpadapter.NewRouter(httpadapter.RouterDeps{
		Logger:      log,
		Service:     svc,
		Checkers:    []httpadapter.PingChecker{pgChecker{pool: pool}, rmqChecker{conn: rmqConn}},
		RateLimiter: limiter,
	})

	server := &http.Server{
		Addr:              ":" + cfg.HTTPPort,
		Handler:           router,
		ReadHeaderTimeout: 5 * time.Second,
	}

	serverErr := make(chan error, 1)
	go func() {
		log.Info("http listening", "port", cfg.HTTPPort)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
		close(serverErr)
	}()

	select {
	case <-rootCtx.Done():
		log.Info("shutdown signal received")
	case err := <-serverErr:
		if err != nil {
			return fmt.Errorf("http server: %w", err)
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Error("http shutdown failed", "err", err)
	}
	log.Info("api stopped")
	return nil
}

type pgChecker struct{ pool *pgxpool.Pool }

func (c pgChecker) Name() string                  { return "postgres" }
func (c pgChecker) Ping(ctx context.Context) error { return c.pool.Ping(ctx) }

type rmqChecker struct{ conn *rabbitmq.Connection }

func (c rmqChecker) Name() string { return "rabbitmq" }
func (c rmqChecker) Ping(_ context.Context) error {
	if c.conn.IsClosed() {
		return errors.New("connection closed")
	}
	return nil
}
