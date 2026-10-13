package api

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// HealthService exposes the readiness check needed by HTTP handlers.
type HealthService interface {
	CheckDatabase(context.Context) error
}

// Handler maps HTTP requests to application services and formats responses.
type Handler struct {
	health HealthService
}

// NewHandler registers the public routes and returns the API HTTP handler.
func NewHandler(health HealthService) http.Handler {
	h := &Handler{health: health}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", h.welcome)
	mux.HandleFunc("GET /healthz", h.liveness)
	mux.HandleFunc("GET /readyz", h.readiness)
	return mux
}

func (h *Handler) welcome(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"service": "company-backend-new",
		"status":  "ok",
	})
}

// liveness reports that the process is running without depending on PostgreSQL.
func (h *Handler) liveness(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// readiness checks the database using a bounded request context.
func (h *Handler) readiness(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := h.health.CheckDatabase(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// writeJSON sends a JSON response with the given HTTP status.
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
