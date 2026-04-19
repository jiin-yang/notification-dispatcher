package http

import (
	"context"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/jiin-yang/notification-dispatcher/internal/platform/metrics"
)

const (
	HeaderCorrelationID = "X-Correlation-ID"
	HeaderRequestID     = "X-Request-ID"
)

type ctxKey int

const (
	ctxKeyCorrelationID ctxKey = iota
	ctxKeyLogger
	ctxKeyRequestID
)

// CorrelationID reads X-Correlation-ID from the request or generates one,
// stores it in context, and echoes it back on the response. The same
// middleware also assigns a per-request X-Request-ID.
func CorrelationID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cid := r.Header.Get(HeaderCorrelationID)
		if _, err := uuid.Parse(cid); err != nil {
			cid = uuid.NewString()
		}
		rid := uuid.NewString()

		w.Header().Set(HeaderCorrelationID, cid)
		w.Header().Set(HeaderRequestID, rid)

		ctx := context.WithValue(r.Context(), ctxKeyCorrelationID, cid)
		ctx = context.WithValue(ctx, ctxKeyRequestID, rid)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func correlationIDFrom(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyCorrelationID).(string); ok {
		return v
	}
	return ""
}

func requestIDFrom(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyRequestID).(string); ok {
		return v
	}
	return ""
}

// RequestLogger attaches a request-scoped slog.Logger with correlation_id and
// request_id and logs each request when it completes.
func RequestLogger(base *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			l := base.With(
				"correlation_id", correlationIDFrom(r.Context()),
				"request_id", requestIDFrom(r.Context()),
			)
			ctx := context.WithValue(r.Context(), ctxKeyLogger, l)

			rw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rw, r.WithContext(ctx))

			l.Info("http request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", rw.status,
				"duration_ms", time.Since(start).Milliseconds(),
			)
		})
	}
}

func loggerFrom(ctx context.Context, fallback *slog.Logger) *slog.Logger {
	if l, ok := ctx.Value(ctxKeyLogger).(*slog.Logger); ok {
		return l
	}
	return fallback
}

// Recoverer catches panics from handlers, logs the stack, and converts them
// into a 500 so the process stays alive.
func Recoverer(base *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					l := loggerFrom(r.Context(), base)
					l.Error("panic recovered",
						"panic", rec,
						"method", r.Method,
						"path", r.URL.Path,
						"stack", string(debug.Stack()),
					)
					if r.Header.Get("Connection") != "Upgrade" {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusInternalServerError)
						_, _ = w.Write([]byte(`{"error":"internal server error"}`))
					}
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// Metrics records HTTP request counters, duration histogram, and in-flight
// gauge. Routing pattern from chi is used as the "route" label to avoid
// unbounded cardinality from raw paths.
func Metrics(m *metrics.Metrics) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if m == nil {
				next.ServeHTTP(w, r)
				return
			}
			m.HTTPRequestsInFlight.Inc()
			defer m.HTTPRequestsInFlight.Dec()

			start := time.Now()
			rw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rw, r)

			route := chi.RouteContext(r.Context()).RoutePattern()
			if route == "" {
				route = "unmatched"
			}
			m.HTTPRequestsTotal.WithLabelValues(r.Method, route, strconv.Itoa(rw.status)).Inc()
			m.HTTPRequestDuration.WithLabelValues(r.Method, route).Observe(time.Since(start).Seconds())
		})
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}
