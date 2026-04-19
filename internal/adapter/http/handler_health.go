package http

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
)

type HealthChecker interface {
	Ping(ctx context.Context) error
	Name() string
}

type HealthHandler struct {
	checkers []HealthChecker
}

func NewHealthHandler(checkers ...HealthChecker) *HealthHandler {
	return &HealthHandler{checkers: checkers}
}

func (h *HealthHandler) Register(r chi.Router) {
	r.Get("/health", h.handle)
}

type healthResponse struct {
	Status     string            `json:"status"`
	Components map[string]string `json:"components,omitempty"`
}

func (h *HealthHandler) handle(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	components := make(map[string]string, len(h.checkers))
	overall := "ok"
	for _, c := range h.checkers {
		if err := c.Ping(ctx); err != nil {
			components[c.Name()] = "down: " + err.Error()
			overall = "degraded"
			continue
		}
		components[c.Name()] = "ok"
	}

	status := http.StatusOK
	if overall != "ok" {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, healthResponse{Status: overall, Components: components})
}
