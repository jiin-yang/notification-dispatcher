package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/jiin-yang/notification-dispatcher/internal/app"
	"github.com/jiin-yang/notification-dispatcher/internal/domain"
	"github.com/jiin-yang/notification-dispatcher/internal/platform/metrics"
)

const (
	maxBatchSize      = 1000
	maxIdempotencyKey = 200
	defaultLimit      = 50
	maxLimit          = 200
)

// NotificationService is the application-layer port this handler depends on.
// Keeping it as a local interface (ISP) means the handler only knows about
// the operations it actually calls.
type NotificationService interface {
	Create(ctx context.Context, in app.CreateInput) (domain.Notification, error)
	CreateWithIdempotency(ctx context.Context, in app.CreateInput, key string) (domain.Notification, bool, error)
	CreateBatch(ctx context.Context, items []app.CreateInput) (batchID uuid.UUID, created []domain.Notification, err error)
	Get(ctx context.Context, id uuid.UUID) (domain.Notification, error)
	GetBatchSummary(ctx context.Context, batchID uuid.UUID) (app.BatchSummary, error)
	List(ctx context.Context, filter app.ListFilter, page, limit int) ([]domain.Notification, int, error)
	Cancel(ctx context.Context, id uuid.UUID) (domain.Notification, error)
}

type NotificationHandler struct {
	svc         NotificationService
	logger      *slog.Logger
	rateLimiter ChannelRateLimiter // may be nil (no limiting)
	metrics     *metrics.Metrics   // may be nil (no metric emission)
}

func NewNotificationHandler(svc NotificationService, logger *slog.Logger, rl ChannelRateLimiter, m *metrics.Metrics) *NotificationHandler {
	return &NotificationHandler{svc: svc, logger: logger, rateLimiter: rl, metrics: m}
}

func (h *NotificationHandler) Register(r chi.Router) {
	r.Post("/notifications", h.create)
	r.Get("/notifications/{messageId}", h.get)
	r.Post("/notifications/batch", h.createBatch)
	r.Get("/notifications/batch/{batchId}", h.getBatchSummary)
	r.Get("/notifications", h.list)
	r.Delete("/notifications/{messageId}", h.cancel)
}

// create handles POST /notifications with optional Idempotency-Key header.
func (h *NotificationHandler) create(w http.ResponseWriter, r *http.Request) {
	log := loggerFrom(r.Context(), h.logger)

	var req createNotificationRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json body")
		return
	}

	if h.rateLimiter != nil {
		if ok, retryAfter := h.rateLimiter.AllowChannel(req.Channel, 1); !ok {
			if h.metrics != nil {
				h.metrics.RateLimitRejected.WithLabelValues(string(req.Channel)).Inc()
			}
			secs := int(math.Ceil(retryAfter.Seconds()))
			w.Header().Set("Retry-After", strconv.Itoa(secs))
			writeError(w, http.StatusTooManyRequests, "rate limit exceeded; try again later")
			return
		}
	}

	cid, err := uuid.Parse(correlationIDFrom(r.Context()))
	if err != nil {
		cid = uuid.New()
	}

	in := app.CreateInput{
		Recipient:     req.To,
		Channel:       req.Channel,
		Content:       req.Content,
		Priority:      req.Priority,
		CorrelationID: cid,
	}

	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))

	var n domain.Notification
	if idempotencyKey != "" {
		if len(idempotencyKey) > maxIdempotencyKey {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("Idempotency-Key must be at most %d characters", maxIdempotencyKey))
			return
		}
		var createErr error
		n, _, createErr = h.svc.CreateWithIdempotency(r.Context(), in, idempotencyKey)
		if createErr != nil {
			if errors.Is(createErr, app.ErrInvalidInput) {
				writeError(w, http.StatusBadRequest, createErr.Error())
				return
			}
			log.Error("create notification with idempotency failed", "err", createErr)
			writeError(w, http.StatusInternalServerError, "failed to create notification")
			return
		}
	} else {
		n, err = h.svc.Create(r.Context(), in)
		if err != nil {
			if errors.Is(err, app.ErrInvalidInput) {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
			log.Error("create notification failed", "err", err)
			writeError(w, http.StatusInternalServerError, "failed to create notification")
			return
		}
	}

	if h.metrics != nil {
		h.metrics.NotificationsEnqueued.WithLabelValues(string(n.Channel), string(n.Priority)).Inc()
	}

	writeJSON(w, http.StatusAccepted, createNotificationResponse{
		MessageID: n.ID,
		Status:    "accepted",
		Timestamp: n.CreatedAt.UTC().Format(time.RFC3339),
	})
}

// get handles GET /notifications/{messageId}.
func (h *NotificationHandler) get(w http.ResponseWriter, r *http.Request) {
	log := loggerFrom(r.Context(), h.logger)

	idStr := chi.URLParam(r, "messageId")
	id, err := uuid.Parse(idStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "messageId must be a uuid")
		return
	}

	n, err := h.svc.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, app.ErrNotFound) {
			writeError(w, http.StatusNotFound, "notification not found")
			return
		}
		log.Error("get notification failed", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to load notification")
		return
	}

	writeJSON(w, http.StatusOK, notificationToResponse(n))
}

// createBatch handles POST /notifications/batch.
func (h *NotificationHandler) createBatch(w http.ResponseWriter, r *http.Request) {
	log := loggerFrom(r.Context(), h.logger)

	var req batchCreateRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 10<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json body")
		return
	}

	if len(req.Notifications) == 0 {
		writeError(w, http.StatusBadRequest, "notifications must not be empty")
		return
	}
	if len(req.Notifications) > maxBatchSize {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("batch exceeds maximum of %d notifications", maxBatchSize))
		return
	}

	// Validate all items upfront; reject the whole batch on any failure.
	var validationErrs []string
	for i, item := range req.Notifications {
		n := domain.Notification{
			Recipient: item.To,
			Channel:   item.Channel,
			Content:   item.Content,
			Priority:  item.Priority,
		}
		if n.Priority == "" {
			n.Priority = domain.PriorityNormal
		}
		if err := n.Validate(); err != nil {
			validationErrs = append(validationErrs, fmt.Sprintf("item %d: %v", i, err))
		}
	}
	if len(validationErrs) > 0 {
		writeJSON(w, http.StatusBadRequest, validationErrorResponse{Errors: validationErrs})
		return
	}

	if h.rateLimiter != nil {
		// Count per-channel, then check each bucket. The whole batch is
		// rejected (all-or-nothing) if any channel's bucket is exhausted.
		channelCounts := make(map[domain.Channel]int, 3)
		for _, item := range req.Notifications {
			channelCounts[item.Channel]++
		}
		var maxRetry time.Duration
		var rateLimited bool
		for ch, n := range channelCounts {
			if ok, retryAfter := h.rateLimiter.AllowChannel(ch, n); !ok {
				rateLimited = true
				if retryAfter > maxRetry {
					maxRetry = retryAfter
				}
			}
		}
		if rateLimited {
			if h.metrics != nil {
				for ch := range channelCounts {
					h.metrics.RateLimitRejected.WithLabelValues(string(ch)).Inc()
				}
			}
			secs := int(math.Ceil(maxRetry.Seconds()))
			w.Header().Set("Retry-After", strconv.Itoa(secs))
			writeError(w, http.StatusTooManyRequests, "rate limit exceeded; try again later")
			return
		}
	}

	cid, err := uuid.Parse(correlationIDFrom(r.Context()))
	if err != nil {
		cid = uuid.New()
	}

	items := make([]app.CreateInput, len(req.Notifications))
	for i, item := range req.Notifications {
		items[i] = app.CreateInput{
			Recipient:     item.To,
			Channel:       item.Channel,
			Content:       item.Content,
			Priority:      item.Priority,
			CorrelationID: cid,
		}
	}

	batchID, created, err := h.svc.CreateBatch(r.Context(), items)
	if err != nil {
		if errors.Is(err, app.ErrInvalidInput) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		log.Error("create batch failed", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to create batch")
		return
	}

	if h.metrics != nil {
		for _, n := range created {
			h.metrics.NotificationsEnqueued.WithLabelValues(string(n.Channel), string(n.Priority)).Inc()
		}
	}

	writeJSON(w, http.StatusAccepted, batchCreateResponse{
		BatchID:   batchID,
		Accepted:  len(created),
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	})
}

// getBatchSummary handles GET /notifications/batch/{batchId}.
func (h *NotificationHandler) getBatchSummary(w http.ResponseWriter, r *http.Request) {
	log := loggerFrom(r.Context(), h.logger)

	idStr := chi.URLParam(r, "batchId")
	batchID, err := uuid.Parse(idStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "batchId must be a uuid")
		return
	}

	summary, err := h.svc.GetBatchSummary(r.Context(), batchID)
	if err != nil {
		if errors.Is(err, app.ErrNotFound) {
			writeError(w, http.StatusNotFound, "batch not found")
			return
		}
		log.Error("get batch summary failed", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to load batch summary")
		return
	}

	writeJSON(w, http.StatusOK, batchSummaryToResponse(summary))
}

// list handles GET /notifications with query-param filters and pagination.
func (h *NotificationHandler) list(w http.ResponseWriter, r *http.Request) {
	log := loggerFrom(r.Context(), h.logger)

	filter, page, limit, parseErr := parseListParams(r)
	if parseErr != "" {
		writeError(w, http.StatusBadRequest, parseErr)
		return
	}

	items, total, err := h.svc.List(r.Context(), filter, page, limit)
	if err != nil {
		log.Error("list notifications failed", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to list notifications")
		return
	}

	resp := listResponse{
		Items: make([]notificationResponse, len(items)),
		Page:  page,
		Limit: limit,
		Total: total,
	}
	for i, n := range items {
		resp.Items[i] = notificationToResponse(n)
	}

	writeJSON(w, http.StatusOK, resp)
}

// cancel handles DELETE /notifications/{messageId}.
func (h *NotificationHandler) cancel(w http.ResponseWriter, r *http.Request) {
	log := loggerFrom(r.Context(), h.logger)

	idStr := chi.URLParam(r, "messageId")
	id, err := uuid.Parse(idStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "messageId must be a uuid")
		return
	}

	n, err := h.svc.Cancel(r.Context(), id)
	if err != nil {
		if errors.Is(err, app.ErrNotFound) {
			writeError(w, http.StatusNotFound, "notification not found")
			return
		}
		if errors.Is(err, app.ErrNotPending) {
			writeError(w, http.StatusConflict, "notification cannot be cancelled: status is not pending")
			return
		}
		log.Error("cancel notification failed", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to cancel notification")
		return
	}

	writeJSON(w, http.StatusOK, cancelResponse{
		MessageID: n.ID,
		Status:    n.Status,
	})
}

// parseListParams parses and validates GET /notifications query parameters.
// Returns a non-empty error string on the first validation failure.
func parseListParams(r *http.Request) (filter app.ListFilter, page, limit int, errMsg string) {
	q := r.URL.Query()

	if statusStr := q.Get("status"); statusStr != "" {
		s := domain.Status(statusStr)
		if !s.Valid() {
			return filter, 0, 0, fmt.Sprintf("unknown status %q; valid values: pending, delivered, failed, cancelled", statusStr)
		}
		filter.Status = s
	}

	if channelStr := q.Get("channel"); channelStr != "" {
		c := domain.Channel(channelStr)
		if !c.Valid() {
			return filter, 0, 0, fmt.Sprintf("unknown channel %q; valid values: sms, email, push", channelStr)
		}
		filter.Channel = c
	}

	if dateFromStr := q.Get("dateFrom"); dateFromStr != "" {
		t, err := time.Parse(time.RFC3339, dateFromStr)
		if err != nil {
			return filter, 0, 0, "dateFrom must be RFC3339 (e.g. 2026-01-01T00:00:00Z)"
		}
		filter.DateFrom = t
	}

	if dateToStr := q.Get("dateTo"); dateToStr != "" {
		t, err := time.Parse(time.RFC3339, dateToStr)
		if err != nil {
			return filter, 0, 0, "dateTo must be RFC3339 (e.g. 2026-12-31T23:59:59Z)"
		}
		filter.DateTo = t
	}

	page = 1
	if pageStr := q.Get("page"); pageStr != "" {
		p, err := strconv.Atoi(pageStr)
		if err != nil || p < 1 {
			return filter, 0, 0, "page must be a positive integer"
		}
		page = p
	}

	limit = defaultLimit
	if limitStr := q.Get("limit"); limitStr != "" {
		l, err := strconv.Atoi(limitStr)
		if err != nil || l < 1 || l > maxLimit {
			return filter, 0, 0, fmt.Sprintf("limit must be between 1 and %d", maxLimit)
		}
		limit = l
	}

	return filter, page, limit, ""
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorResponse{Error: msg})
}
