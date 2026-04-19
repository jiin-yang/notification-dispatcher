package http

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/jiin-yang/notification-dispatcher/internal/platform/metrics"
)

// RegisterMetrics mounts /metrics backed by the service's own registry.
func RegisterMetrics(r chi.Router, m *metrics.Metrics) {
	if m == nil {
		return
	}
	handler := promhttp.HandlerFor(m.Registry(), promhttp.HandlerOpts{
		ErrorHandling: promhttp.ContinueOnError,
	})
	r.Method(http.MethodGet, "/metrics", handler)
}
