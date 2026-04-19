package config

import (
	"fmt"
	"time"

	"github.com/caarlos0/env/v11"
)

type Config struct {
	HTTPPort        string        `env:"HTTP_PORT"                envDefault:"8080"`
	DBDSN           string        `env:"DB_DSN,required"`
	RMQURL          string        `env:"RMQ_URL,required"`
	WebhookURL      string        `env:"WEBHOOK_URL,required"`
	WebhookTimeout  time.Duration `env:"WEBHOOK_TIMEOUT"          envDefault:"10s"`
	LogLevel        string        `env:"LOG_LEVEL"                envDefault:"info"`
	ShutdownTimeout time.Duration `env:"SHUTDOWN_TIMEOUT"         envDefault:"30s"`
	// Per-channel in-process rate limiting (Phase 3).
	RateLimitPerSecond float64 `env:"RATE_LIMIT_PER_SECOND"    envDefault:"100"`
	RateLimitBurst     int     `env:"RATE_LIMIT_BURST"         envDefault:"200"`
}

func Load() (Config, error) {
	var cfg Config
	if err := env.Parse(&cfg); err != nil {
		return Config{}, fmt.Errorf("parse env: %w", err)
	}
	return cfg, nil
}
