package integration

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"company/backend/internal/api"
	"company/backend/internal/codex"
	"company/backend/internal/repository"
	"company/backend/internal/service"

	_ "github.com/jackc/pgx/v5/stdlib"
)

const defaultIntegrationDatabaseURL = "postgres://company:company@localhost:5432/thecompany?sslmode=disable"

// TestRegistrationLoginAndCodexConversation exercises account creation, login,
// explicit container-backed agent creation, two turns on its thread, and cleanup.
// t provides test configuration and reports failures; database URL overrides are optional.
func TestRegistrationLoginAndCodexConversation(t *testing.T) {
	databaseURL := isolatedDatabaseURL(t)
	db, err := repository.OpenPostgres(databaseURL)
	if err != nil {
		t.Fatalf("open isolated database: %v", err)
	}
	defer db.Close()

	auth, err := service.NewAuthService(db, "integration-test-signing-secret-32-bytes-minimum")
	if err != nil {
		t.Fatalf("create authentication service: %v", err)
	}
	t.Setenv("CODEX_AGENT_DATA_DIR", t.TempDir())
	codexManager, err := codex.NewPodmanManager(os.Getenv("CODEX_PODMAN_IMAGE"))
	if err != nil {
		t.Fatalf("create Podman Codex manager: %v", err)
	}
	var ownerID int64
	var agentName string
	t.Cleanup(func() {
		if ownerID != 0 && agentName != "" {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			if err := codexManager.RemoveAgent(cleanupCtx, ownerID, agentName); err != nil {
				t.Errorf("remove test agent container: %v", err)
			}
			cancel()
		}
		if err := codexManager.Close(); err != nil {
			t.Errorf("close Podman Codex manager: %v", err)
		}
	})

	handler := api.NewHandlerWithServices(service.NewHealthService(db), codexManager, db, auth)
	server := httptest.NewServer(handler)
	defer server.Close()
	client := &http.Client{Timeout: 90 * time.Second}

	var registration struct {
		User struct {
			ID    int64  `json:"id"`
			Email string `json:"email"`
			Role  string `json:"role"`
		} `json:"user"`
		Token string `json:"token"`
	}
	email := fmt.Sprintf("test-%d@example.test", time.Now().UnixNano())
	t.Log("registering test user")
	if status := postJSON(t, client, server.URL+"/auth/register", map[string]string{
		"email": email, "password": "integration-test-password",
	}, "", &registration); status != http.StatusCreated {
		t.Fatalf("registration status = %d, want %d", status, http.StatusCreated)
	}
	if registration.User.ID == 0 || registration.User.Role != "user" || registration.Token == "" {
		t.Fatalf("registration returned incomplete user or token: %+v", registration.User)
	}

	var login struct {
		User struct {
			ID   int64  `json:"id"`
			Role string `json:"role"`
		} `json:"user"`
		Token string `json:"token"`
	}
	t.Log("logging in test user")
	if status := postJSON(t, client, server.URL+"/auth/login", map[string]string{
		"email": email, "password": "integration-test-password",
	}, "", &login); status != http.StatusOK {
		t.Fatalf("login status = %d, want %d", status, http.StatusOK)
	}
	if login.User.ID != registration.User.ID || login.User.Role != "user" || login.Token == "" {
		t.Fatalf("login returned unexpected account or token: %+v", login.User)
	}
	ownerID = login.User.ID

	var createdAgent struct {
		AgentName     string `json:"agent_name"`
		ThreadID      string `json:"thread_id"`
		ContainerName string `json:"container_name"`
	}
	t.Log("creating a container-backed Codex agent")
	if status := postJSON(t, client, server.URL+"/codex/agents", map[string]string{}, login.Token, &createdAgent); status != http.StatusCreated {
		t.Fatalf("agent creation status = %d, want %d", status, http.StatusCreated)
	}
	agentName = createdAgent.AgentName
	if len(agentName) != 3 || createdAgent.ThreadID == "" || createdAgent.ContainerName == "" {
		t.Fatalf("agent creation returned incomplete identifiers: %+v", createdAgent)
	}

	var firstMessage struct {
		Response string `json:"response"`
		Agent    string `json:"agent_name"`
		Error    string `json:"error"`
	}
	t.Log("sending first message to Codex app-server")
	if status := postJSON(t, client, server.URL+"/codex/messages", map[string]string{
		"message": "Reply with exactly READY. Do not use tools.", "agent_name": agentName,
	}, login.Token, &firstMessage); status != http.StatusOK {
		podman := os.Getenv("PODMAN_BIN")
		if podman == "" {
			podman = "podman"
		}
		logsCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		logs, logsErr := exec.CommandContext(logsCtx, podman, "logs", createdAgent.ContainerName).CombinedOutput()
		cancel()
		t.Fatalf("first Codex message status = %d, want %d (error %q; container logs error %v: %s)", status, http.StatusOK, firstMessage.Error, logsErr, strings.TrimSpace(string(logs)))
	}
	if firstMessage.Response == "" || len(firstMessage.Agent) != 3 {
		t.Fatalf("first Codex response or agent name is empty: %+v", firstMessage)
	}

	var secondMessage struct {
		Response string `json:"response"`
		Agent    string `json:"agent_name"`
	}
	t.Log("sending follow-up message to Codex app-server")
	if status := postJSON(t, client, server.URL+"/codex/messages", map[string]string{
		"message": "Reply with exactly SECOND. Do not use tools.", "agent_name": firstMessage.Agent,
	}, login.Token, &secondMessage); status != http.StatusOK {
		t.Fatalf("second Codex message status = %d, want %d", status, http.StatusOK)
	}
	if secondMessage.Response == "" || secondMessage.Agent != firstMessage.Agent {
		t.Fatalf("second message did not continue the first agent: %+v", secondMessage)
	}

	if firstMessage.Agent != agentName {
		t.Fatalf("message used agent %q, want %q", firstMessage.Agent, agentName)
	}
	removeRequest, err := http.NewRequest(http.MethodDelete, server.URL+"/codex/agents/"+agentName, nil)
	if err != nil {
		t.Fatalf("create agent cleanup request: %v", err)
	}
	removeRequest.Header.Set("Authorization", "Bearer "+login.Token)
	removeResponse, err := client.Do(removeRequest)
	if err != nil {
		t.Fatalf("delete test agent: %v", err)
	}
	removeResponse.Body.Close()
	if removeResponse.StatusCode != http.StatusNoContent {
		t.Fatalf("agent deletion status = %d, want %d", removeResponse.StatusCode, http.StatusNoContent)
	}
	agentName = ""
}

// isolatedDatabaseURL creates an empty schema for the integration test and
// returns a PostgreSQL URL whose connections use that schema as search_path.
// t reports setup failures; INTEGRATION_TEST_DATABASE_URL optionally overrides the local default URL.
func isolatedDatabaseURL(t *testing.T) string {
	t.Helper()
	databaseURL := os.Getenv("INTEGRATION_TEST_DATABASE_URL")
	if databaseURL == "" {
		databaseURL = os.Getenv("DATABASE_URL")
	}
	if databaseURL == "" {
		databaseURL = defaultIntegrationDatabaseURL
	}
	adminDB, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatalf("open integration database: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := adminDB.PingContext(ctx); err != nil {
		adminDB.Close()
		t.Fatalf("connect to integration database: %v", err)
	}
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		adminDB.Close()
		t.Fatalf("generate isolated schema name: %v", err)
	}
	schema := "codex_test_" + hex.EncodeToString(suffix[:])
	if _, err := adminDB.ExecContext(ctx, `CREATE SCHEMA "`+schema+`"`); err != nil {
		adminDB.Close()
		t.Fatalf("create isolated schema: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		if _, err := adminDB.ExecContext(cleanupCtx, `DROP SCHEMA "`+schema+`" CASCADE`); err != nil {
			t.Errorf("drop isolated integration schema: %v", err)
		}
		if err := adminDB.Close(); err != nil {
			t.Errorf("close integration database connection: %v", err)
		}
	})
	parsedURL, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatalf("parse integration database URL: %v", err)
	}
	query := parsedURL.Query()
	query.Set("search_path", schema)
	parsedURL.RawQuery = query.Encode()
	return parsedURL.String()
}

// postJSON sends value as JSON with an optional bearer token, checks transport
// errors, decodes the response into target, and returns the HTTP status.
// t reports failures; client performs the request; endpoint is the full URL;
// value is the JSON body; token is optional; target receives decoded JSON.
func postJSON(t *testing.T, client *http.Client, endpoint string, value any, token string, target any) int {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode request for %s: %v", endpoint, err)
	}
	request, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("create request for %s: %v", endpoint, err)
	}
	request.Header.Set("Content-Type", "application/json")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("send request to %s: %v", endpoint, err)
	}
	defer response.Body.Close()
	if err := json.NewDecoder(response.Body).Decode(target); err != nil {
		t.Fatalf("decode response from %s: %v", endpoint, err)
	}
	return response.StatusCode
}
