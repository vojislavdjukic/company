package codex

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// PodmanManager owns one Podman container and WebSocket client per app agent.
// podman is the executable; image is the image containing the Codex CLI;
// dataDir stores each agent's persistent Codex home; authFile supplies the
// local machine's existing Codex login; hostUID and hostGID own mounted data.
type PodmanManager struct {
	mu       sync.Mutex
	podman   string
	image    string
	dataDir  string
	authFile string
	hostUID  int
	hostGID  int
	agents   map[string]*managedAgent
	removed  map[string]struct{}
	closed   bool
}

// managedAgent holds the persistent container identity and its connected client.
// name is the Podman name; client is the WebSocket session for its app-server.
type managedAgent struct {
	name   string
	client *Client
}

// ContainerInfo contains the safe, user-facing fields returned for one
// app-managed Codex container. ID and name identify it; owner and agent name
// describe its association; status and createdAt describe its Podman state.
type ContainerInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name,omitempty"`
	Description string `json:"description,omitempty"`
	AgentName   string `json:"agent_name"`
	OwnerID     int64  `json:"owner_id"`
	Status      string `json:"status"`
	CreatedAt   string `json:"created_at"`
}

// podmanContainer is the subset of Podman's JSON list output used to build
// ContainerInfo. Labels carry the manager's owner and agent identity.
type podmanContainer struct {
	ID        string            `json:"Id"`
	Names     []string          `json:"Names"`
	State     string            `json:"State"`
	Status    string            `json:"Status"`
	CreatedAt string            `json:"CreatedAt"`
	Labels    map[string]string `json:"Labels"`
}

// NewPodmanManager configures container management from the supplied image and
// environment overrides. image is the Codex container image; PODMAN_BIN,
// CODEX_AGENT_DATA_DIR, and CODEX_AUTH_FILE optionally select runtime paths.
func NewPodmanManager(image string) (*PodmanManager, error) {
	if image == "" {
		image = os.Getenv("CODEX_PODMAN_IMAGE")
	}
	if image == "" {
		image = "localhost/thecomp-codex:latest"
	}
	podman := os.Getenv("PODMAN_BIN")
	if podman == "" {
		podman = "podman"
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("find home directory for Codex credentials: %w", err)
	}
	dataDir := os.Getenv("CODEX_AGENT_DATA_DIR")
	if dataDir == "" {
		dataDir = filepath.Join(home, ".local", "share", "thecomp", "agents")
	}
	dataDir, err = filepath.Abs(dataDir)
	if err != nil {
		return nil, fmt.Errorf("resolve Codex agent data directory: %w", err)
	}
	authFile := os.Getenv("CODEX_AUTH_FILE")
	if authFile == "" {
		authFile = filepath.Join(home, ".codex", "auth.json")
	}
	return &PodmanManager{
		podman: podman, image: image, dataDir: dataDir, authFile: authFile,
		hostUID: os.Getuid(), hostGID: os.Getgid(), agents: make(map[string]*managedAgent), removed: make(map[string]struct{}),
	}, nil
}

// SendAgent sends a prompt to an agent's container. ctx bounds startup and the
// turn; userID and agentName select the container; message and threadID select
// the turn; developerInstructions seed a replacement thread if history is gone.
func (m *PodmanManager) SendAgent(ctx context.Context, userID int64, agentName, message, threadID, developerInstructions string) (string, string, error) {
	agent, err := m.ensureAgent(ctx, userID, agentName)
	if err != nil {
		return "", "", err
	}
	return agent.client.Send(ctx, message, threadID, developerInstructions)
}

// EnsureAgent creates and connects an agent's container without sending a
// prompt. ctx bounds startup; userID and agentName choose its isolated container.
func (m *PodmanManager) EnsureAgent(ctx context.Context, userID int64, agentName string) error {
	_, err := m.ensureAgent(ctx, userID, agentName)
	return err
}

// CreateAgent starts the user's container and creates its first Codex thread.
// ctx bounds startup; userID and agentName identify it; developerInstructions
// become the app-server's developer instructions for that thread.
func (m *PodmanManager) CreateAgent(ctx context.Context, userID int64, agentName, developerInstructions string) (string, string, error) {
	agent, err := m.ensureAgent(ctx, userID, agentName)
	if err != nil {
		return "", "", err
	}
	threadID, err := agent.client.StartThread(ctx, developerInstructions)
	if err != nil {
		return "", agent.name, err
	}
	return threadID, agent.name, nil
}

// ensureAgent returns the connected container for one user and agent, starting
// it when needed. ctx bounds startup; userID and agentName identify the agent.
func (m *PodmanManager) ensureAgent(ctx context.Context, userID int64, agentName string) (*managedAgent, error) {
	if !isThreeLetterAgentName(agentName) {
		return nil, errors.New("agent name must contain three letters before starting its container")
	}
	key := fmt.Sprintf("%d:%s", userID, agentName)
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, errors.New("Podman Codex manager is closed")
	}
	if _, wasRemoved := m.removed[key]; wasRemoved {
		m.mu.Unlock()
		return nil, errors.New("agent has been removed")
	}
	agent := m.agents[key]
	if agent == nil {
		var err error
		agent, err = m.startAgent(ctx, userID, agentName)
		if err != nil {
			m.mu.Unlock()
			return nil, err
		}
		m.agents[key] = agent
	}
	m.mu.Unlock()
	return agent, nil
}

// RemoveAgent stops and removes one user's container and persistent Codex home.
// ctx bounds Podman commands; userID and agentName select the container and data.
func (m *PodmanManager) RemoveAgent(ctx context.Context, userID int64, agentName string) error {
	if !isThreeLetterAgentName(agentName) {
		return errors.New("agent name must contain three letters")
	}
	key := fmt.Sprintf("%d:%s", userID, agentName)
	name := fmt.Sprintf("thecomp-u%d-%s", userID, agentName)
	dataDir := filepath.Join(m.dataDir, fmt.Sprintf("u%d", userID), agentName)
	m.mu.Lock()
	defer m.mu.Unlock()
	_, removeErr := m.podmanOutput(ctx, "rm", "--force", name)
	if removeErr != nil && !isContainerMissing(removeErr) {
		return fmt.Errorf("remove Podman Codex container %s: %w", name, removeErr)
	}
	m.removed[key] = struct{}{}
	var closeErr error
	if agent := m.agents[key]; agent != nil {
		closeErr = agent.client.Close()
		delete(m.agents, key)
	}
	if err := os.RemoveAll(dataDir); err != nil {
		return errors.Join(closeErr, fmt.Errorf("remove persistent Codex home for %s: %w", name, err))
	}
	if closeErr != nil {
		return fmt.Errorf("close Codex connection for removed agent %s: %w", name, closeErr)
	}
	return nil
}

// ListAgents returns all managed containers for administrators or only those
// owned by userID for regular users. ctx bounds the Podman query; isAdmin
// selects whether ownership filtering is applied.
func (m *PodmanManager) ListAgents(ctx context.Context, userID int64, isAdmin bool) ([]ContainerInfo, error) {
	output, err := m.podmanOutput(ctx, "ps", "--all", "--filter", "label=thecomp.managed=true", "--format", "json")
	if err != nil {
		return nil, fmt.Errorf("list Podman Codex containers: %w", err)
	}
	var listed []podmanContainer
	if strings.TrimSpace(output) == "" {
		return []ContainerInfo{}, nil
	}
	if err := json.Unmarshal([]byte(output), &listed); err != nil {
		return nil, fmt.Errorf("decode Podman container list: %w", err)
	}
	containers := make([]ContainerInfo, 0, len(listed))
	for _, item := range listed {
		if item.Labels["thecomp.managed"] != "true" {
			continue
		}
		ownerID, err := strconv.ParseInt(item.Labels["thecomp.user_id"], 10, 64)
		if err != nil || (!isAdmin && ownerID != userID) {
			continue
		}
		agentName := item.Labels["thecomp.agent_name"]
		if !isThreeLetterAgentName(agentName) || len(item.Names) == 0 {
			continue
		}
		status := item.State
		if status == "" {
			status = item.Status
		}
		containers = append(containers, ContainerInfo{
			ID: item.ID, Name: item.Names[0], AgentName: agentName,
			OwnerID: ownerID, Status: status, CreatedAt: item.CreatedAt,
		})
	}
	sort.Slice(containers, func(i, j int) bool {
		if containers[i].CreatedAt == containers[j].CreatedAt {
			return containers[i].Name < containers[j].Name
		}
		return containers[i].CreatedAt < containers[j].CreatedAt
	})
	return containers, nil
}

// isThreeLetterAgentName checks the format used in stable per-agent container
// names. agentName is the caller-facing three-letter identifier.
func isThreeLetterAgentName(agentName string) bool {
	if len(agentName) != 3 {
		return false
	}
	for _, letter := range agentName {
		if letter < 'a' || letter > 'z' {
			return false
		}
	}
	return true
}

// isContainerMissing reports whether Podman failed to remove a name that
// already has no container. err is the wrapped output from a Podman command.
func isContainerMissing(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "no such container") || strings.Contains(message, "no container with name") || strings.Contains(message, "not found")
}

// startAgent creates or restarts a named per-agent container and connects its
// app-server. ctx bounds Podman commands; userID and agentName determine the
// stable container name and isolated persistent home directory.
func (m *PodmanManager) startAgent(ctx context.Context, userID int64, agentName string) (*managedAgent, error) {
	name := fmt.Sprintf("thecomp-u%d-%s", userID, agentName)
	dataDir := filepath.Join(m.dataDir, fmt.Sprintf("u%d", userID), agentName)
	if err := m.prepareAgentHome(dataDir); err != nil {
		return nil, err
	}
	wsToken, err := m.agentWebSocketToken(dataDir)
	if err != nil {
		return nil, err
	}
	inspect, err := m.podmanOutput(ctx, "inspect", "--format", "{{.State.Running}}", name)
	if err != nil {
		args := []string{
			"run", "-d", "--name", name,
			"--label", "thecomp.managed=true",
			"--label", "thecomp.user_id=" + strconv.FormatInt(userID, 10),
			"--label", "thecomp.agent_name=" + agentName,
			"--userns=keep-id", "--user", fmt.Sprintf("%d:%d", m.hostUID, m.hostGID),
			"-p", "127.0.0.1::8765/tcp",
			"-e", "HOME=/codex-home",
			"-v", dataDir + ":/codex-home:Z",
			m.image, "app-server", "--listen", "ws://0.0.0.0:8765",
			"--ws-auth", "capability-token", "--ws-token-file", "/codex-home/.codex/ws-token",
		}
		if _, runErr := m.podmanOutput(ctx, args...); runErr != nil {
			return nil, fmt.Errorf("create Podman Codex container %s: %w", name, runErr)
		}
	} else if strings.TrimSpace(inspect) != "true" {
		if _, err := m.podmanOutput(ctx, "start", name); err != nil {
			return nil, fmt.Errorf("start Podman Codex container %s: %w", name, err)
		}
	}
	port, err := m.podmanOutput(ctx, "port", name, "8765/tcp")
	if err != nil {
		return nil, fmt.Errorf("find WebSocket port for Podman container %s: %w", name, err)
	}
	port = strings.TrimSpace(port)
	if lastColon := strings.LastIndex(port, ":"); lastColon >= 0 {
		port = port[lastColon+1:]
	}
	if _, err := strconv.Atoi(port); err != nil {
		return nil, fmt.Errorf("Podman returned an invalid published port for %s: %q", name, port)
	}
	listenURL := (&url.URL{Scheme: "ws", Host: "127.0.0.1:" + port}).String()
	client := NewRemoteClientWithToken(listenURL, wsToken)
	connectCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	client.mu.Lock()
	err = client.start(connectCtx)
	client.mu.Unlock()
	if err != nil {
		_ = client.Close()
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, cleanupErr := m.podmanOutput(cleanupCtx, "stop", "--time", "5", name)
		cleanupCancel()
		if cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("stop container after app-server connection failure: %w", cleanupErr))
		}
		return nil, fmt.Errorf("connect to Codex app-server in %s: %w", name, err)
	}
	return &managedAgent{name: name, client: client}, nil
}

// agentWebSocketToken loads or creates a per-agent app-server transport token.
// dataDir is the persistent home mounted into the agent container.
func (m *PodmanManager) agentWebSocketToken(dataDir string) (string, error) {
	tokenPath := filepath.Join(dataDir, ".codex", "ws-token")
	if token, err := os.ReadFile(tokenPath); err == nil {
		return strings.TrimSpace(string(token)), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("read per-agent app-server token: %w", err)
	}
	var randomToken [32]byte
	if _, err := rand.Read(randomToken[:]); err != nil {
		return "", fmt.Errorf("generate per-agent app-server token: %w", err)
	}
	token := hex.EncodeToString(randomToken[:])
	if err := os.WriteFile(tokenPath, []byte(token), 0o600); err != nil {
		return "", fmt.Errorf("write per-agent app-server token: %w", err)
	}
	return token, nil
}

// prepareAgentHome creates isolated persistent Codex state and copies the
// current local login once. dataDir is mounted into the container as its home.
func (m *PodmanManager) prepareAgentHome(dataDir string) error {
	codexDir := filepath.Join(dataDir, ".codex")
	if err := os.MkdirAll(codexDir, 0o700); err != nil {
		return fmt.Errorf("create per-agent Codex home: %w", err)
	}
	localAuth := filepath.Join(codexDir, "auth.json")
	if _, err := os.Stat(localAuth); err == nil {
		return nil
	}
	contents, err := os.ReadFile(m.authFile)
	if err != nil {
		return fmt.Errorf("read local Codex login at %s: %w", m.authFile, err)
	}
	if err := os.WriteFile(localAuth, contents, 0o600); err != nil {
		return fmt.Errorf("copy local Codex login into per-agent home: %w", err)
	}
	return nil
}

// podmanOutput runs one Podman CLI command and returns stdout. ctx limits the
// command; args are passed directly as arguments without shell interpretation.
func (m *PodmanManager) podmanOutput(ctx context.Context, args ...string) (string, error) {
	command := exec.CommandContext(ctx, m.podman, args...)
	output, err := command.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("podman %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return string(output), nil
}

// Close disconnects every WebSocket and stops every container this manager has
// opened. Persistent container homes remain on disk for later thread resumption.
func (m *PodmanManager) Close() error {
	m.mu.Lock()
	agents := make([]*managedAgent, 0, len(m.agents))
	for _, agent := range m.agents {
		agents = append(agents, agent)
	}
	m.agents = make(map[string]*managedAgent)
	m.closed = true
	m.mu.Unlock()
	var closeErr error
	for _, agent := range agents {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, err := m.podmanOutput(ctx, "stop", "--time", "5", agent.name)
		if err != nil {
			_, killErr := m.podmanOutput(ctx, "kill", agent.name)
			if killErr != nil {
				closeErr = errors.Join(closeErr, fmt.Errorf("stop Podman Codex container %s: %w", agent.name, err), fmt.Errorf("kill Podman Codex container %s: %w", agent.name, killErr))
			}
		}
		cancel()
		if clientErr := agent.client.Close(); clientErr != nil {
			closeErr = errors.Join(closeErr, clientErr)
		}
	}
	return closeErr
}
