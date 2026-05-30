package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/tbarnesjr/squamish-events/internal/db"
	"github.com/tbarnesjr/squamish-events/internal/models"
)

// Handler provides HTTP handlers for the API.
type Handler struct {
	db         *db.DB
	adminToken string
}

// New creates a new API handler.
func New(database *db.DB) *Handler {
	return &Handler{
		db:         database,
		adminToken: os.Getenv("ADMIN_TOKEN"),
	}
}

// RegisterRoutes registers all API routes on the given mux.
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/events", h.listEvents)
	mux.HandleFunc("GET /api/events/{id}", h.getEvent)
	mux.HandleFunc("GET /api/sources", h.listSources)
	mux.HandleFunc("GET /api/categories", h.listCategories)
	mux.HandleFunc("POST /api/submissions", h.createSubmission)

	// Admin endpoints
	mux.HandleFunc("GET /api/admin/submissions", h.adminAuth(h.listSubmissions))
	mux.HandleFunc("POST /api/admin/submissions/{id}/approve", h.adminAuth(h.approveSubmission))
	mux.HandleFunc("POST /api/admin/submissions/{id}/reject", h.adminAuth(h.rejectSubmission))
	mux.HandleFunc("POST /api/admin/sources", h.adminAuth(h.createSource))
}

func (h *Handler) adminAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.adminToken == "" {
			jsonError(w, "admin token not configured", http.StatusInternalServerError)
			return
		}

		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") || strings.TrimPrefix(auth, "Bearer ") != h.adminToken {
			jsonError(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		next(w, r)
	}
}

func (h *Handler) listEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := db.EventFilter{
		Search:   q.Get("search"),
		Category: q.Get("category"),
		Source:   q.Get("source"),
	}

	if v := q.Get("from"); v != "" {
		t, err := time.Parse("2006-01-02", v)
		if err == nil {
			filter.From = &t
		}
	}
	if v := q.Get("to"); v != "" {
		t, err := time.Parse("2006-01-02", v)
		if err == nil {
			filter.To = &t
		}
	}
	if v := q.Get("limit"); v != "" {
		n, _ := strconv.Atoi(v)
		filter.Limit = n
	}
	if v := q.Get("offset"); v != "" {
		n, _ := strconv.Atoi(v)
		filter.Offset = n
	}

	events, err := h.db.ListEvents(r.Context(), filter)
	if err != nil {
		slog.Error("api: list events failed", "error", err)
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	jsonOK(w, events)
}

func (h *Handler) getEvent(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		jsonError(w, "invalid id", http.StatusBadRequest)
		return
	}

	event, err := h.db.GetEvent(r.Context(), id)
	if err != nil {
		slog.Error("api: get event failed", "error", err)
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	if event == nil {
		jsonError(w, "not found", http.StatusNotFound)
		return
	}

	jsonOK(w, event)
}

func (h *Handler) listSources(w http.ResponseWriter, r *http.Request) {
	sources, err := h.db.ListSources(r.Context())
	if err != nil {
		slog.Error("api: list sources failed", "error", err)
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	jsonOK(w, sources)
}

func (h *Handler) listCategories(w http.ResponseWriter, _ *http.Request) {
	categories := []string{
		"Community",
		"Sports",
		"Arts",
		"Music",
		"Outdoors",
		"Education",
		"Food & Drink",
		"Family",
		"Other",
	}
	jsonOK(w, categories)
}

func (h *Handler) createSubmission(w http.ResponseWriter, r *http.Request) {
	var sub models.Submission
	if err := json.NewDecoder(r.Body).Decode(&sub); err != nil {
		jsonError(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	if sub.Title == "" {
		jsonError(w, "title is required", http.StatusBadRequest)
		return
	}
	if sub.StartTime.IsZero() {
		jsonError(w, "start_time is required", http.StatusBadRequest)
		return
	}

	sub.Status = "pending"
	id, err := h.db.CreateSubmission(r.Context(), sub)
	if err != nil {
		slog.Error("api: create submission failed", "error", err)
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	jsonOK(w, map[string]int64{"id": id})
}

func (h *Handler) listSubmissions(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	if status == "" {
		status = "pending"
	}

	subs, err := h.db.ListSubmissions(r.Context(), status)
	if err != nil {
		slog.Error("api: list submissions failed", "error", err)
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	jsonOK(w, subs)
}

func (h *Handler) approveSubmission(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		jsonError(w, "invalid id", http.StatusBadRequest)
		return
	}

	sub, err := h.db.GetSubmission(r.Context(), id)
	if err != nil {
		slog.Error("api: get submission failed", "error", err)
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	if sub == nil {
		jsonError(w, "not found", http.StatusNotFound)
		return
	}
	if sub.Status != "pending" {
		jsonError(w, "submission already processed", http.StatusConflict)
		return
	}

	// Copy to events table — create a "community" source if needed
	event := models.Event{
		SourceID:    0, // will be set to community source
		ExternalID:  "submission-" + strconv.FormatInt(id, 10),
		Title:       sub.Title,
		Description: sub.Description,
		Location:    sub.Location,
		URL:         sub.URL,
		StartTime:   sub.StartTime,
		EndTime:      sub.EndTime,
		Category:    sub.Category,
	}

	// Get or create a community submissions source
	sourceID, err := h.getOrCreateCommunitySource(r.Context())
	if err != nil {
		slog.Error("api: create community source failed", "error", err)
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	event.SourceID = sourceID

	if err := h.db.UpsertEvent(r.Context(), event); err != nil {
		slog.Error("api: upsert approved event failed", "error", err)
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	if err := h.db.UpdateSubmissionStatus(r.Context(), id, "approved"); err != nil {
		slog.Error("api: update submission status failed", "error", err)
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	jsonOK(w, map[string]string{"status": "approved"})
}

func (h *Handler) rejectSubmission(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		jsonError(w, "invalid id", http.StatusBadRequest)
		return
	}

	sub, err := h.db.GetSubmission(r.Context(), id)
	if err != nil {
		slog.Error("api: get submission failed", "error", err)
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	if sub == nil {
		jsonError(w, "not found", http.StatusNotFound)
		return
	}
	if sub.Status != "pending" {
		jsonError(w, "submission already processed", http.StatusConflict)
		return
	}

	if err := h.db.UpdateSubmissionStatus(r.Context(), id, "rejected"); err != nil {
		slog.Error("api: update submission status failed", "error", err)
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	jsonOK(w, map[string]string{"status": "rejected"})
}

func (h *Handler) createSource(w http.ResponseWriter, r *http.Request) {
	var src models.Source
	if err := json.NewDecoder(r.Body).Decode(&src); err != nil {
		jsonError(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	if src.Name == "" || src.Type == "" || src.URL == "" {
		jsonError(w, "name, type, and url are required", http.StatusBadRequest)
		return
	}

	id, err := h.db.CreateSource(r.Context(), src)
	if err != nil {
		slog.Error("api: create source failed", "error", err)
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	jsonOK(w, map[string]int64{"id": id})
}

func (h *Handler) getOrCreateCommunitySource(ctx context.Context) (int64, error) {
	sources, err := h.db.ListSources(ctx)
	if err != nil {
		return 0, err
	}

	for _, s := range sources {
		if s.Name == "Community Submissions" {
			return s.ID, nil
		}
	}

	return h.db.CreateSource(ctx, models.Source{
		Name:           "Community Submissions",
		Type:           "community",
		URL:            "",
		ScrapeInterval: 0,
		Enabled:        false,
		Config:         "{}",
	})
}

func jsonOK(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
