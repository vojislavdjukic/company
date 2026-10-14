package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"company/backend/internal/codex"
)

// HealthService exposes the readiness check needed by HTTP handlers.
type HealthService interface {
	CheckDatabase(context.Context) error
}

// CodexService sends a prompt to a Codex app-server conversation.
type CodexService interface {
	Send(context.Context, string, string) (string, string, error)
}

// AgentRegistry maps caller-facing names to Codex thread IDs. ctx controls the
// database operation; names are three lowercase letters and thread IDs are
// returned by Codex or persisted in PostgreSQL.
type AgentRegistry interface {
	LookupAgentThread(context.Context, string) (string, error)
	CreateAgentThread(context.Context, string) (string, error)
	ReplaceAgentThread(context.Context, string, string, string) (string, error)
}

// Handler maps HTTP requests to application services and formats responses.
type Handler struct {
	health HealthService
	codex  CodexService
	agents AgentRegistry
}

// NewHandler registers the public routes and returns the API HTTP handler.
// health supplies the readiness check; an optional codexService sends agent
// prompts. Use NewHandlerWithAgentRegistry to enable named Codex conversations.
func NewHandler(health HealthService, codexService ...CodexService) http.Handler {
	var codexClient CodexService
	if len(codexService) > 0 {
		codexClient = codexService[0]
	}
	return NewHandlerWithAgentRegistry(health, codexClient, nil)
}

// NewHandlerWithAgentRegistry constructs the API with health, Codex, and agent
// name dependencies. health checks PostgreSQL; codexClient sends prompts;
// agents persist short-name-to-thread-ID mappings.
func NewHandlerWithAgentRegistry(health HealthService, codexClient CodexService, agents AgentRegistry) http.Handler {
	h := &Handler{health: health, codex: codexClient, agents: agents}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", h.welcome)
	mux.HandleFunc("GET /healthz", h.liveness)
	mux.HandleFunc("GET /readyz", h.readiness)
	mux.HandleFunc("POST /codex/messages", h.sendCodexMessage)
	return mux
}

type codexMessageRequest struct {
	// Message is the user's new turn. AgentName is an optional name returned by
	// an earlier response; omitting it creates a new conversation.
	Message   string `json:"message"`
	AgentName string `json:"agent_name,omitempty"`
}

// Path: POST /codex/messages
// sendCodexMessage accepts a prompt and optional agent name, sends it to Codex,
// and returns the response and resolved name. w receives the HTTP response; r
// contains the JSON request body and cancellation context.
func (h *Handler) sendCodexMessage(w http.ResponseWriter, r *http.Request) {
	if h.codex == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Codex is unavailable"})
		return
	}
	if h.agents == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "agent registry is unavailable"})
		return
	}
	// Bound request size before decoding so a large body cannot consume
	// unbounded memory in this synchronous endpoint.
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var request codexMessageRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || strings.TrimSpace(request.Message) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "provide a non-empty message"})
		return
	}
	agentName := strings.ToLower(strings.TrimSpace(request.AgentName))
	if agentName != "" && !validAgentName(agentName) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "agent_name must be three letters"})
		return
	}

	// A Codex turn may take longer than a normal API call; cap it at two minutes
	// and also stop waiting if the caller disconnects.
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	threadID := ""
	if agentName != "" {
		var err error
		threadID, err = h.agents.LookupAgentThread(ctx, agentName)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "look up agent name: " + err.Error()})
			return
		}
	}

	response, resultThreadID, err := h.codex.Send(ctx, request.Message, threadID)
	if errors.Is(err, codex.ErrThreadNotFound) && agentName != "" {
		// A saved alias can outlive a deleted thread. Start a fresh conversation
		// and atomically replace the stale mapping with a newly generated name.
		response, resultThreadID, err = h.codex.Send(ctx, request.Message, "")
		if resultThreadID != "" {
			newName, registryErr := h.agents.ReplaceAgentThread(ctx, agentName, threadID, resultThreadID)
			if registryErr != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "replace stale agent mapping: " + registryErr.Error()})
				return
			}
			agentName = newName
		}
	} else if resultThreadID != "" && threadID == "" {
		// New conversations receive a unique generated alias for future requests.
		newName, registryErr := h.agents.CreateAgentThread(ctx, resultThreadID)
		if registryErr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "store agent mapping: " + registryErr.Error()})
			return
		}
		agentName = newName
	}
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error(), "agent_name": agentName})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"response": response, "agent_name": agentName})
}

// validAgentName checks that name matches the lowercase three-letter aliases
// issued by the registry. name is already trimmed and normalized by the handler.
func validAgentName(name string) bool {
	if len(name) != 3 {
		return false
	}
	for _, char := range name {
		if char < 'a' || char > 'z' {
			return false
		}
	}
	return true
}

// Path: GET /
// welcome returns the service name and status. w receives the HTTP response;
// r provides the request path used to reject unmatched root paths.
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

// Path: GET /healthz
// liveness reports that the process is running without checking PostgreSQL.
// w receives the HTTP response; the request is unused.
func (h *Handler) liveness(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// Path: GET /readyz
// readiness checks database readiness using a bounded request context. w
// receives the HTTP response; r supplies the context for the database ping.
func (h *Handler) readiness(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := h.health.CheckDatabase(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// writeJSON sends value as a JSON response with status. w is the response
// writer, status is the HTTP status code, and value is the JSON-encoded body.
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
