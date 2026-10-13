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

// CodexService sends a prompt to a Codex app-server conversation.
type CodexService interface {
	Send(context.Context, string, string) (string, string, error)
}

// Handler maps HTTP requests to application services and formats responses.
type Handler struct {
	health HealthService
	codex  CodexService
}

// NewHandler registers the public routes and returns the API HTTP handler.
func NewHandler(health HealthService, codexService ...CodexService) http.Handler {
	h := &Handler{health: health}
	if len(codexService) > 0 {
		h.codex = codexService[0]
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", h.welcome)
	mux.HandleFunc("GET /healthz", h.liveness)
	mux.HandleFunc("GET /readyz", h.readiness)
	mux.HandleFunc("POST /codex/messages", h.sendCodexMessage)
	return mux
}

type codexMessageRequest struct {
	// Message is the new user turn. ThreadID is optional: omit it to begin a
	// fresh conversation, or pass back the ID returned by a previous response.
	Message  string `json:"message"`
	ThreadID string `json:"thread_id,omitempty"`
}

func (h *Handler) sendCodexMessage(w http.ResponseWriter, r *http.Request) {
	if h.codex == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Codex is unavailable"})
		return
	}
	// Bound request size before decoding so a large body cannot consume
	// unbounded memory in this synchronous endpoint.
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var request codexMessageRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Message == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "provide a non-empty message"})
		return
	}
	// A Codex turn may take longer than a normal API call; cap it at two minutes
	// and also stop waiting if the caller disconnects.
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	response, threadID, err := h.codex.Send(ctx, request.Message, request.ThreadID)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	// Return both the text and the conversation handle. The caller should retain
	// this ID per chat and include it in its next POST to continue context.
	writeJSON(w, http.StatusOK, map[string]string{"response": response, "thread_id": threadID})
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
