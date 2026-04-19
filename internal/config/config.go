package config

import (
	"fmt"
	"time"

	"github.com/caarlos0/env/v11"
)

type Config struct {
	HTTPPort        string        `env:"HTTP_PORT"                envDefault:"8080"`
	MetricsPort     string        `env:"METRICS_PORT"             envDefault:"9090"`
	DBDSN           string        `env:"DB_DSN,required"`
	RMQURL          string        `env:"RMQ_URL,required"`
	WebhookURL      string        `env:"WEBHOOK_URL,required"`
	WebhookTimeout  time.Duration `env:"WEBHOOK_TIMEOUT"          envDefault:"10s"`
	LogLevel        string        `env:"LOG_LEVEL"                envDefault:"info"`
	ShutdownTimeout time.Duration `env:"SHUTDOWN_TIMEOUT"         envDefault:"30s"`

	// Per-channel in-process rate limiting (Phase 3).
	RateLimitPerSecond float64 `env:"RATE_LIMIT_PER_SECOND"    envDefault:"100"`
	RateLimitBurst     int     `env:"RATE_LIMIT_BURST"         envDefault:"200"`

	// Circuit breaker settings (Phase 4). One breaker per channel.
	CBFailureThreshold  uint32        `env:"CB_FAILURE_THRESHOLD"     envDefault:"5"`
	CBOpenDuration      time.Duration `env:"CB_OPEN_DURATION"         envDefault:"30s"`
	CBHalfOpenMaxCalls  uint32        `env:"CB_HALF_OPEN_MAX_CALLS"   envDefault:"2"`
}

func Load() (Config, error) {
	var cfg Config
	if err := env.Parse(&cfg); err != nil {
		return Config{}, fmt.Errorf("parse env: %w", err)
	}
	return cfg, nil
}
