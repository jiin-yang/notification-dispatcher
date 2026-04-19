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
}

func Load() (Config, error) {
	var cfg Config
	if err := env.Parse(&cfg); err != nil {
		return Config{}, fmt.Errorf("parse env: %w", err)
	}
	return cfg, nil
}
