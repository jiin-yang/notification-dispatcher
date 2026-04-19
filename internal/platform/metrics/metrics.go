// Package metrics centralizes Prometheus collectors used by the api and the
// worker. Collectors are registered on a dedicated Registry so the two
// processes can each expose /metrics from their own Gatherer without fighting
// over the default global registry.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// Metrics bundles every Prometheus collector the services expose. Fields are
// public so hot paths can increment without going through a setter.
type Metrics struct {
	reg *prometheus.Registry

	// HTTP server metrics.
	HTTPRequestsTotal    *prometheus.CounterVec
	HTTPRequestDuration  *prometheus.HistogramVec
	HTTPRequestsInFlight prometheus.Gauge

	// Business metrics — API side.
	NotificationsEnqueued *prometheus.CounterVec // labels: channel, priority
	RateLimitRejected     *prometheus.CounterVec // labels: channel

	// Business metrics — worker side.
	NotificationsDelivered *prometheus.CounterVec // labels: channel, outcome
	NotificationsRetried   *prometheus.CounterVec // labels: channel, next_attempt
	NotificationsDLQ       *prometheus.CounterVec // labels: channel, reason
	DeliveryDuration       *prometheus.HistogramVec
	NotificationsInFlight  prometheus.Gauge

	CircuitBreakerState *prometheus.GaugeVec // labels: channel; 0=closed 1=half_open 2=open

	// Worker supervisor metrics.
	ConsumerRestarts *prometheus.CounterVec // labels: queue
}

// New constructs a Metrics bundle with its own registry. service is exposed
// via a constant label so the same dashboards can distinguish api vs worker.
func New(service string) *Metrics {
	reg := prometheus.NewRegistry()

	constLabels := prometheus.Labels{"service": service}

	m := &Metrics{
		reg: reg,

		HTTPRequestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "http_requests_total",
			Help:        "Total number of HTTP requests processed, labeled by method, route, and status.",
			ConstLabels: constLabels,
		}, []string{"method", "route", "status"}),

		HTTPRequestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:        "http_request_duration_seconds",
			Help:        "Duration of HTTP requests in seconds.",
			Buckets:     prometheus.DefBuckets,
			ConstLabels: constLabels,
		}, []string{"method", "route"}),

		HTTPRequestsInFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name:        "http_requests_in_flight",
			Help:        "Number of HTTP requests currently being served.",
			ConstLabels: constLabels,
		}),

		NotificationsEnqueued: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "notifications_enqueued_total",
			Help:        "Notifications accepted by the API and published to RabbitMQ.",
			ConstLabels: constLabels,
		}, []string{"channel", "priority"}),

		RateLimitRejected: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "rate_limit_rejected_total",
			Help:        "Requests rejected by the per-channel rate limiter.",
			ConstLabels: constLabels,
		}, []string{"channel"}),

		NotificationsDelivered: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "notifications_delivered_total",
			Help:        "Notification delivery outcomes.",
			ConstLabels: constLabels,
		}, []string{"channel", "outcome"}),

		NotificationsRetried: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "notifications_retried_total",
			Help:        "Notifications routed back to the retry exchange.",
			ConstLabels: constLabels,
		}, []string{"channel", "next_attempt"}),

		NotificationsDLQ: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "notifications_dlq_total",
			Help:        "Notifications routed to the dead-letter exchange.",
			ConstLabels: constLabels,
		}, []string{"channel", "reason"}),

		DeliveryDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:        "notification_delivery_duration_seconds",
			Help:        "Time spent in the provider Send call.",
			Buckets:     []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
			ConstLabels: constLabels,
		}, []string{"channel", "outcome"}),

		NotificationsInFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name:        "notifications_in_flight",
			Help:        "Messages currently being processed by the worker.",
			ConstLabels: constLabels,
		}),

		CircuitBreakerState: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name:        "circuit_breaker_state",
			Help:        "Circuit breaker state per channel (0=closed, 1=half_open, 2=open).",
			ConstLabels: constLabels,
		}, []string{"channel"}),

		ConsumerRestarts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "consumer_restarts_total",
			Help:        "Number of times a consumer supervisor restarted the consumer after a failure.",
			ConstLabels: constLabels,
		}, []string{"queue"}),
	}

	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.HTTPRequestsTotal,
		m.HTTPRequestDuration,
		m.HTTPRequestsInFlight,
		m.NotificationsEnqueued,
		m.RateLimitRejected,
		m.NotificationsDelivered,
		m.NotificationsRetried,
		m.NotificationsDLQ,
		m.DeliveryDuration,
		m.NotificationsInFlight,
		m.CircuitBreakerState,
		m.ConsumerRestarts,
	)

	return m
}

// Registry returns the underlying Prometheus registry. Use it to build the
// /metrics HTTP handler.
func (m *Metrics) Registry() *prometheus.Registry { return m.reg }
