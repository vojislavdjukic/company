package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"company/backend/internal/codex"
	"company/backend/internal/service"
)

// HealthService exposes the readiness check needed by HTTP handlers.
type HealthService interface {
	CheckDatabase(context.Context) error
}

// AgentCodexService sends a prompt through the runtime assigned to one account
// and agent name. userID and agentName select the runtime; message and threadID
// identify the requested Codex turn.
type AgentCodexService interface {
	SendAgent(context.Context, int64, string, string, string, string) (string, string, error)
}

// AgentCreator starts a user's isolated container and creates its first thread.
// ctx bounds the operation; userID owns the agent and agentName identifies it.
type AgentCreator interface {
	CreateAgent(context.Context, int64, string, string) (string, string, error)
}

// AgentEnsurer starts a newly reserved agent container without creating a thread.
// ctx bounds startup; userID and agentName identify the agent.
type AgentEnsurer interface {
	EnsureAgent(context.Context, int64, string) error
}

// AgentStarter explicitly starts a stopped agent container.
// ctx bounds startup; userID and agentName select the owned agent.
type AgentStarter interface {
	StartAgent(context.Context, int64, string) error
}

// AgentNameAllocator reserves an internal alias with optional user-facing
// metadata and saves the first thread ID. ctx bounds database work; userID
// owns the alias; threadID is returned by Codex.
type AgentNameAllocator interface {
	ReserveAgentName(context.Context, int64) (string, error)
	ReserveNamedAgent(context.Context, int64, string, string) (string, error)
	SetAgentThread(context.Context, int64, string, string) error
	ReplaceAgentThreadID(context.Context, int64, string, string, string) error
}

// AgentMetadataRegistry loads the user-provided label and developer instructions.
// ctx bounds database work; userID scopes ownership and agentName is the short ID.
type AgentMetadataRegistry interface {
	LookupAgentMetadata(context.Context, int64, string) (service.AgentProfile, error)
	ListAgentMetadata(context.Context, int64, bool) ([]service.AgentProfile, error)
}

// AgentChatHistory persists and reads messages for an owned agent. ctx bounds
// database work; userID scopes ownership; agentName identifies the chat; role
// selects the speaker; content is the message text.
type AgentChatHistory interface {
	SaveAgentMessage(context.Context, int64, string, string, string) (service.AgentMessage, error)
	ListAgentMessages(context.Context, int64, string) ([]service.AgentMessage, error)
}

// AgentLifecycleRegistry checks ownership and removes an agent's saved mapping.
// ctx bounds database work; userID owns the agent; agentName identifies it.
type AgentLifecycleRegistry interface {
	HasAgent(context.Context, int64, string) (bool, error)
	DeleteAgent(context.Context, int64, string) (bool, error)
}

// AgentRemover stops and removes one user's agent runtime and its persistent
// data. ctx bounds cleanup; userID and agentName select the owned agent.
type AgentRemover interface {
	RemoveAgent(context.Context, int64, string) error
}

// AgentStopper stops one user's agent container while keeping its saved state.
// ctx bounds cleanup; userID and agentName select the container to stop.
type AgentStopper interface {
	StopAgent(context.Context, int64, string) error
}

// AgentLister returns the app-managed containers visible to the given account.
// ctx bounds the Podman query; userID scopes regular users; isAdmin enables the
// system-wide view for administrators.
type AgentLister interface {
	ListAgents(context.Context, int64, bool) ([]codex.ContainerInfo, error)
}

// AgentRegistry maps caller-facing names to Codex thread IDs. ctx controls the
// database operation; names are three lowercase letters and thread IDs are
// returned by Codex or persisted in PostgreSQL.
type AgentRegistry interface {
	LookupAgentThread(context.Context, int64, string) (string, error)
}

// AccountService performs account registration, login, and bearer-token validation.
type AccountService interface {
	Register(context.Context, string, string) (service.User, string, error)
	Login(context.Context, string, string) (service.User, string, error)
	Authenticate(context.Context, string) (service.User, error)
}

// Handler maps HTTP requests to application services and formats responses.
type Handler struct {
	health HealthService
	codex  any
	agents AgentRegistry
	auth   AccountService
}

// NewHandler registers the public routes and returns the API HTTP handler.
// health supplies the readiness check; an optional codexService sends agent
// prompts. Use NewHandlerWithAgentRegistry to enable named Codex conversations.
func NewHandler(health HealthService, codexService ...any) http.Handler {
	var codexClient any
	if len(codexService) > 0 {
		codexClient = codexService[0]
	}
	return NewHandlerWithAgentRegistry(health, codexClient, nil)
}

// NewHandlerWithAgentRegistry constructs the API with health, Codex, and agent
// name dependencies. health checks PostgreSQL; codexClient sends prompts;
// agents persist short-name-to-thread-ID mappings.
func NewHandlerWithAgentRegistry(health HealthService, codexClient any, agents AgentRegistry) http.Handler {
	return NewHandlerWithServices(health, codexClient, agents, nil)
}

// NewHandlerWithServices builds the API with health, Codex, registry, and account services.
// health checks readiness; codexClient sends prompts; agents persist thread aliases; auth manages accounts.
func NewHandlerWithServices(health HealthService, codexClient any, agents AgentRegistry, auth AccountService) http.Handler {
	h := &Handler{health: health, codex: codexClient, agents: agents, auth: auth}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", h.welcome)
	mux.HandleFunc("GET /healthz", h.liveness)
	mux.HandleFunc("GET /readyz", h.readiness)
	mux.HandleFunc("POST /codex/messages", h.sendCodexMessage)
	mux.HandleFunc("POST /codex/agents", h.createCodexAgent)
	mux.HandleFunc("GET /codex/agents", h.listCodexAgents)
	mux.HandleFunc("GET /codex/agents/{agent_name}/messages", h.listCodexAgentMessages)
	mux.HandleFunc("POST /codex/agents/{agent_name}/start", h.startCodexAgent)
	mux.HandleFunc("POST /codex/agents/{agent_name}/stop", h.stopCodexAgent)
	mux.HandleFunc("DELETE /codex/agents/{agent_name}", h.deleteCodexAgent)
	mux.HandleFunc("POST /auth/register", h.register)
	mux.HandleFunc("POST /auth/login", h.login)
	mux.HandleFunc("GET /auth/me", h.currentUser)
	return mux
}

// Path: POST /codex/agents
// createCodexAgent reserves an alias and profile, starts its container, and
// creates the initial instructed thread. w receives identifiers; r has the profile and token.
func (h *Handler) createCodexAgent(w http.ResponseWriter, r *http.Request) {
	user, ok := h.authenticatedUser(w, r)
	if !ok {
		return
	}
	if user.Role != "user" && user.Role != "admin" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "role cannot create Codex agents"})
		return
	}
	var request createCodexAgentRequest
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "provide a name and description"})
		return
	}
	request.Name = strings.TrimSpace(request.Name)
	request.Description = strings.TrimSpace(request.Description)
	if request.Name == "" || len([]rune(request.Name)) > 64 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name must contain between 1 and 64 characters"})
		return
	}
	if request.Description == "" || len([]rune(request.Description)) > 4000 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "description must contain between 1 and 4000 characters"})
		return
	}
	allocator, ok := h.agents.(AgentNameAllocator)
	if !ok {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "agent name allocator is unavailable"})
		return
	}
	registry, ok := h.agents.(AgentLifecycleRegistry)
	if !ok {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "agent cleanup registry is unavailable"})
		return
	}
	creator, ok := h.codex.(AgentCreator)
	if !ok {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "container-backed Codex service is unavailable"})
		return
	}
	remover, ok := h.codex.(AgentRemover)
	if !ok {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "agent container cleanup is unavailable"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	agentName, err := allocator.ReserveNamedAgent(ctx, user.ID, request.Name, request.Description)
	if err != nil {
		if errors.Is(err, service.ErrAgentNameTaken) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "an agent with this name already exists"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "reserve agent name: " + err.Error()})
		return
	}
	rollback := func() error {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cleanupCancel()
		removeErr := remover.RemoveAgent(cleanupCtx, user.ID, agentName)
		_, deleteErr := registry.DeleteAgent(cleanupCtx, user.ID, agentName)
		if removeErr != nil {
			removeErr = errors.Join(errors.New("remove failed agent container"), removeErr)
		}
		if deleteErr != nil {
			deleteErr = errors.Join(errors.New("remove failed agent mapping"), deleteErr)
		}
		return errors.Join(removeErr, deleteErr)
	}
	threadID, containerName, err := creator.CreateAgent(ctx, user.ID, agentName, request.Description)
	if err != nil {
		rollbackErr := rollback()
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": errors.Join(fmt.Errorf("create container-backed agent: %w", err), rollbackErr).Error()})
		return
	}
	if err := allocator.SetAgentThread(ctx, user.ID, agentName, threadID); err != nil {
		rollbackErr := rollback()
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": errors.Join(fmt.Errorf("store agent thread: %w", err), rollbackErr).Error()})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{
		"agent_name": agentName, "name": request.Name, "description": request.Description,
		"thread_id": threadID, "container_name": containerName,
	})
}

type createCodexAgentRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// Path: GET /codex/agents
// listCodexAgents returns managed containers visible to the authenticated
// account. w receives the JSON list; r supplies the bearer token and context.
func (h *Handler) listCodexAgents(w http.ResponseWriter, r *http.Request) {
	user, ok := h.authenticatedUser(w, r)
	if !ok {
		return
	}
	if user.Role != "user" && user.Role != "admin" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "role cannot list Codex containers"})
		return
	}
	lister, ok := h.codex.(AgentLister)
	if !ok {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "container listing is unavailable"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	containers, err := lister.ListAgents(ctx, user.ID, user.Role == "admin")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "list Codex containers: " + err.Error()})
		return
	}
	if metadata, ok := h.agents.(AgentMetadataRegistry); ok {
		profiles, err := metadata.ListAgentMetadata(ctx, user.ID, user.Role == "admin")
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "list agent profiles: " + err.Error()})
			return
		}
		byAgent := make(map[string]service.AgentProfile, len(profiles))
		for _, profile := range profiles {
			byAgent[profile.AgentName] = profile
		}
		for i := range containers {
			profile := byAgent[containers[i].AgentName]
			containers[i].DisplayName = profile.Name
			containers[i].Description = profile.Description
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"containers": containers})
}

// Path: DELETE /codex/agents/{agent_name}
// deleteCodexAgent removes the authenticated user's agent, including its
// container and saved thread mapping. w receives the status; r supplies the
// agent name and authentication token.
func (h *Handler) deleteCodexAgent(w http.ResponseWriter, r *http.Request) {
	user, ok := h.authenticatedUser(w, r)
	if !ok {
		return
	}
	agentName := strings.ToLower(strings.TrimSpace(r.PathValue("agent_name")))
	if !validAgentName(agentName) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "agent_name must be three letters"})
		return
	}
	registry, ok := h.agents.(AgentLifecycleRegistry)
	if !ok {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "agent lifecycle registry is unavailable"})
		return
	}
	remover, ok := h.codex.(AgentRemover)
	if !ok {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "agent runtime removal is unavailable"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	exists, err := registry.HasAgent(ctx, user.ID, agentName)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "look up agent: " + err.Error()})
		return
	}
	if !exists {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "agent not found"})
		return
	}
	if err := remover.RemoveAgent(ctx, user.ID, agentName); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "remove agent runtime: " + err.Error()})
		return
	}
	deleted, err := registry.DeleteAgent(ctx, user.ID, agentName)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "remove agent mapping: " + err.Error()})
		return
	}
	if !deleted {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "agent not found"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Path: POST /codex/agents/{agent_name}/stop
// stopCodexAgent stops an owned agent container while preserving its mapping
// and chat history. w receives the status; r supplies the agent name and token.
func (h *Handler) stopCodexAgent(w http.ResponseWriter, r *http.Request) {
	user, ok := h.authenticatedUser(w, r)
	if !ok {
		return
	}
	agentName := strings.ToLower(strings.TrimSpace(r.PathValue("agent_name")))
	if !validAgentName(agentName) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "agent_name must be three letters"})
		return
	}
	registry, ok := h.agents.(AgentLifecycleRegistry)
	if !ok {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "agent lifecycle registry is unavailable"})
		return
	}
	stopper, ok := h.codex.(AgentStopper)
	if !ok {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "agent runtime stop is unavailable"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	exists, err := registry.HasAgent(ctx, user.ID, agentName)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "look up agent: " + err.Error()})
		return
	}
	if !exists {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "agent not found"})
		return
	}
	if err := stopper.StopAgent(ctx, user.ID, agentName); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "stop agent runtime: " + err.Error()})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Path: POST /codex/agents/{agent_name}/start
// startCodexAgent explicitly starts one owned agent container. w receives the
// status; r supplies the agent name and bearer token.
func (h *Handler) startCodexAgent(w http.ResponseWriter, r *http.Request) {
	user, ok := h.authenticatedUser(w, r)
	if !ok {
		return
	}
	agentName := strings.ToLower(strings.TrimSpace(r.PathValue("agent_name")))
	if !validAgentName(agentName) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "agent_name must be three letters"})
		return
	}
	registry, ok := h.agents.(AgentLifecycleRegistry)
	if !ok {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "agent lifecycle registry is unavailable"})
		return
	}
	starter, ok := h.codex.(AgentStarter)
	if !ok {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "agent runtime start is unavailable"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	exists, err := registry.HasAgent(ctx, user.ID, agentName)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "look up agent: " + err.Error()})
		return
	}
	if !exists {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "agent not found"})
		return
	}
	if err := starter.StartAgent(ctx, user.ID, agentName); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "start agent runtime: " + err.Error()})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type codexMessageRequest struct {
	// Message is the user's new turn. AgentName is an optional name returned by
	// an earlier response; omitting it creates a new conversation.
	Message   string `json:"message"`
	AgentName string `json:"agent_name,omitempty"`
}

// Path: GET /codex/agents/{agent_name}/messages
// listCodexAgentMessages returns one owned agent's messages in send order. w
// receives the history; r supplies the bearer token and three-letter alias.
func (h *Handler) listCodexAgentMessages(w http.ResponseWriter, r *http.Request) {
	user, ok := h.authenticatedUser(w, r)
	if !ok {
		return
	}
	agentName := strings.ToLower(strings.TrimSpace(r.PathValue("agent_name")))
	if !validAgentName(agentName) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "agent_name must be three letters"})
		return
	}
	registry, ok := h.agents.(AgentLifecycleRegistry)
	if !ok {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "agent registry is unavailable"})
		return
	}
	history, ok := h.agents.(AgentChatHistory)
	if !ok {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "chat history is unavailable"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	exists, err := registry.HasAgent(ctx, user.ID, agentName)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "look up agent: " + err.Error()})
		return
	}
	if !exists {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "agent not found"})
		return
	}
	messages, err := history.ListAgentMessages(ctx, user.ID, agentName)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "load chat history: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"agent_name": agentName, "messages": messages})
}

// Path: POST /codex/messages
// sendCodexMessage accepts a prompt and optional agent name, sends it to Codex,
// and returns the response and resolved name. w receives the HTTP response; r
// contains the JSON request body and cancellation context.
func (h *Handler) sendCodexMessage(w http.ResponseWriter, r *http.Request) {
	user, ok := h.authenticatedUser(w, r)
	if !ok {
		return
	}
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
	developerInstructions := ""
	if agentName != "" {
		var err error
		threadID, err = h.agents.LookupAgentThread(ctx, user.ID, agentName)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "look up agent name: " + err.Error()})
			return
		}
		if metadata, ok := h.agents.(AgentMetadataRegistry); ok {
			profile, profileErr := metadata.LookupAgentMetadata(ctx, user.ID, agentName)
			if profileErr != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "look up agent profile: " + profileErr.Error()})
				return
			}
			developerInstructions = profile.Description
		}
	}
	managedCodex, isManaged := h.codex.(AgentCodexService)
	allocator, canReserve := h.agents.(AgentNameAllocator)
	if !isManaged {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "container-backed Codex service is unavailable"})
		return
	}
	if !canReserve {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "agent name allocator is unavailable"})
		return
	}
	reserveGeneratedName := agentName == ""
	if agentName != "" && threadID == "" {
		lifecycle, canCheckAgent := h.agents.(AgentLifecycleRegistry)
		if !canCheckAgent {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "agent registry is unavailable"})
			return
		}
		exists, existsErr := lifecycle.HasAgent(ctx, user.ID, agentName)
		if existsErr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "look up agent name: " + existsErr.Error()})
			return
		}
		reserveGeneratedName = !exists
	}
	if reserveGeneratedName {
		reservedName, reserveErr := allocator.ReserveAgentName(ctx, user.ID)
		if reserveErr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "reserve agent name: " + reserveErr.Error()})
			return
		}
		agentName = reservedName
		threadID = ""
		ensurer, canEnsure := h.codex.(AgentEnsurer)
		if !canEnsure {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "agent container startup is unavailable"})
			return
		}
		if err := ensurer.EnsureAgent(ctx, user.ID, agentName); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "start new agent container: " + err.Error()})
			return
		}
	}
	history, canStoreHistory := h.agents.(AgentChatHistory)
	if !canStoreHistory {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "chat history is unavailable"})
		return
	}
	needsThreadSave := threadID == ""
	staleThreadID := ""
	response, resultThreadID, err := managedCodex.SendAgent(ctx, user.ID, agentName, request.Message, threadID, developerInstructions)
	if errors.Is(err, codex.ErrThreadNotFound) && agentName != "" {
		needsThreadSave = true
		staleThreadID = threadID
		response, resultThreadID, err = managedCodex.SendAgent(ctx, user.ID, agentName, request.Message, "", developerInstructions)
	}
	if errors.Is(err, codex.ErrAgentStopped) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error(), "agent_name": agentName})
		return
	}
	if _, storeErr := history.SaveAgentMessage(ctx, user.ID, agentName, "user", request.Message); storeErr != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "store user message: " + storeErr.Error()})
		return
	}
	if err == nil {
		if _, storeErr := history.SaveAgentMessage(ctx, user.ID, agentName, "assistant", response); storeErr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "store Codex reply: " + storeErr.Error(), "agent_name": agentName})
			return
		}
	}
	if resultThreadID != "" && needsThreadSave {
		var registryErr error
		if staleThreadID != "" {
			registryErr = allocator.ReplaceAgentThreadID(ctx, user.ID, agentName, staleThreadID, resultThreadID)
		} else {
			registryErr = allocator.SetAgentThread(ctx, user.ID, agentName, resultThreadID)
		}
		if registryErr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "store agent thread: " + registryErr.Error()})
			return
		}
	}
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error(), "agent_name": agentName})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"response": response, "agent_name": agentName})
}

type credentialsRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// Path: POST /auth/register
// register creates an account and returns its bearer token. w receives the response; r contains credentials.
func (h *Handler) register(w http.ResponseWriter, r *http.Request) {
	if h.auth == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "authentication is unavailable"})
		return
	}
	var credentials credentialsRequest
	if !decodeCredentials(w, r, &credentials) {
		return
	}
	user, token, err := h.auth.Register(r.Context(), credentials.Email, credentials.Password)
	if errors.Is(err, service.ErrInvalidInput) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if errors.Is(err, service.ErrEmailTaken) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "register account"})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"user": user, "token": token})
}

// Path: POST /auth/login
// login verifies credentials and returns a new bearer token. w receives the response; r contains credentials.
func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	if h.auth == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "authentication is unavailable"})
		return
	}
	var credentials credentialsRequest
	if !decodeCredentials(w, r, &credentials) {
		return
	}
	user, token, err := h.auth.Login(r.Context(), credentials.Email, credentials.Password)
	if errors.Is(err, service.ErrInvalidCredentials) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid email or password"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "log in"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": user, "token": token})
}

// Path: GET /auth/me
// currentUser returns the account belonging to the supplied bearer token. w receives the response; r supplies Authorization.
func (h *Handler) currentUser(w http.ResponseWriter, r *http.Request) {
	user, ok := h.authenticatedUser(w, r)
	if ok {
		writeJSON(w, http.StatusOK, user)
	}
}

// decodeCredentials decodes a bounded JSON credentials body. w receives any validation response; r contains the body; target receives decoded values.
func decodeCredentials(w http.ResponseWriter, r *http.Request, target *credentialsRequest) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(target); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "provide valid email and password JSON"})
		return false
	}
	return true
}

// authenticatedUser validates the request's Bearer token. w receives an error response when invalid; r supplies Authorization.
func (h *Handler) authenticatedUser(w http.ResponseWriter, r *http.Request) (service.User, bool) {
	if h.auth == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "authentication is unavailable"})
		return service.User{}, false
	}
	parts := strings.Fields(r.Header.Get("Authorization"))
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "provide a bearer token"})
		return service.User{}, false
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	user, err := h.auth.Authenticate(ctx, parts[1])
	if err != nil {
		if errors.Is(err, service.ErrInvalidToken) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
		} else {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "authenticate bearer token"})
		}
		return service.User{}, false
	}
	return user, true
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
