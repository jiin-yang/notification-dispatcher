package http

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/jiin-yang/notification-dispatcher/internal/adapter/postgres"
	"github.com/jiin-yang/notification-dispatcher/internal/app"
	"github.com/jiin-yang/notification-dispatcher/internal/domain"
)

type NotificationService interface {
	Create(ctx context.Context, in app.CreateInput) (domain.Notification, error)
	Get(ctx context.Context, id uuid.UUID) (domain.Notification, error)
}

type NotificationHandler struct {
	svc    NotificationService
	logger *slog.Logger
}

func NewNotificationHandler(svc NotificationService, logger *slog.Logger) *NotificationHandler {
	return &NotificationHandler{svc: svc, logger: logger}
}

func (h *NotificationHandler) Register(r chi.Router) {
	r.Post("/notifications", h.create)
	r.Get("/notifications/{messageId}", h.get)
}

func (h *NotificationHandler) create(w http.ResponseWriter, r *http.Request) {
	log := loggerFrom(r.Context(), h.logger)

	var req createNotificationRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json body")
		return
	}

	cid, err := uuid.Parse(correlationIDFrom(r.Context()))
	if err != nil {
		cid = uuid.New()
	}

	n, err := h.svc.Create(r.Context(), app.CreateInput{
		Recipient:     req.To,
		Channel:       req.Channel,
		Content:       req.Content,
		Priority:      req.Priority,
		CorrelationID: cid,
	})
	if err != nil {
		if errors.Is(err, app.ErrInvalidInput) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		log.Error("create notification failed", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to create notification")
		return
	}

	writeJSON(w, http.StatusAccepted, createNotificationResponse{
		MessageID: n.ID,
		Status:    "accepted",
		Timestamp: n.CreatedAt.UTC().Format(time.RFC3339),
	})
}

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
		if errors.Is(err, postgres.ErrNotFound) {
			writeError(w, http.StatusNotFound, "notification not found")
			return
		}
		log.Error("get notification failed", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to load notification")
		return
	}

	writeJSON(w, http.StatusOK, notificationToResponse(n))
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorResponse{Error: msg})
}
