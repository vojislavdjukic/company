package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// ErrThreadNotFound indicates that a saved agent name points to a Codex thread
// that app-server can no longer resume.
var ErrThreadNotFound = errors.New("Codex thread not found")

// Client owns a Codex app-server WebSocket and optionally its local process.
// Calls are serialized so response events from concurrent turns do not mix.
type Client struct {
	mu          sync.Mutex
	command     string
	listenURL   string
	wsToken     string
	cmd         *exec.Cmd
	conn        *websocket.Conn
	processDone chan struct{}
	processErr  error
	seq         int
	started     bool
	pending     [][]byte
	threads     map[string]struct{}
	external    bool
}

// NewRemoteClient creates a client that connects to an already running Codex
// app-server. listenURL is the server's WebSocket address; the remote process
// is managed by the container manager rather than this client.
func NewRemoteClient(listenURL string) *Client {
	return NewRemoteClientWithToken(listenURL, "")
}

// NewRemoteClientWithToken creates a client for a remote app-server using its
// capability token. listenURL is the server's WebSocket address; wsToken is
// the separate transport credential required by an authenticated listener.
func NewRemoteClientWithToken(listenURL, wsToken string) *Client {
	if listenURL == "" {
		listenURL = "ws://127.0.0.1:8765"
	}
	return &Client{listenURL: listenURL, wsToken: wsToken, threads: make(map[string]struct{}), external: true}
}

// NewClient creates a Codex client that launches app-server and connects to its
// WebSocket listener. command is the Codex executable path (or "codex" when
// empty); listenURL is the server bind address and client URL (or localhost
// port 8765 when empty).
func NewClient(command, listenURL string) *Client {
	if command == "" {
		command = "codex"
	}
	if listenURL == "" {
		listenURL = "ws://127.0.0.1:8765"
	}
	return &Client{
		command:   command,
		listenURL: listenURL,
		threads:   make(map[string]struct{}),
	}
}

// Send submits a prompt and waits for the completed response. ctx bounds the
// operation; message is the prompt; threadID selects the conversation; and
// developerInstructions apply when this call must start a fresh thread.
func (c *Client) Send(ctx context.Context, message, threadID, developerInstructions string) (string, string, error) {
	// Only one turn may read this shared WebSocket at a time; concurrent HTTP
	// requests wait here until the current Codex turn has completed.
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.start(ctx); err != nil {
		return "", "", err
	}
	if err := c.initialize(ctx); err != nil {
		return "", "", err
	}

	var thread map[string]json.RawMessage
	var err error
	if threadID == "" {
		// A missing ID represents a new chat, while a supplied ID reconnects the
		// caller to existing Codex history.
		params := map[string]any{}
		if developerInstructions != "" {
			params["developerInstructions"] = developerInstructions
		}
		thread, err = c.request(ctx, "thread/start", params)
	} else {
		thread, err = c.request(ctx, "thread/resume", map[string]any{"threadId": threadID})
	}
	if err != nil {
		if threadID != "" && isMissingThreadError(err) {
			return "", threadID, ErrThreadNotFound
		}
		return "", "", fmt.Errorf("start Codex thread: %w", err)
	}
	if threadID == "" {
		// Return Codex's new thread ID to the API caller so it can be reused on
		// later requests for this same chat.
		var info struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(thread["thread"], &info); err != nil || info.ID == "" {
			return "", "", errors.New("Codex returned no thread ID")
		}
		threadID = info.ID
	}
	// Remember every thread this process has opened, including resumed threads,
	// so Close can release its app-server subscription during shutdown.
	c.threads[threadID] = struct{}{}

	// A turn is one prompt and its reply inside a thread. Keep its ID so events
	// from any other activity on the connection cannot enter this response.
	turn, err := c.request(ctx, "turn/start", map[string]any{
		"threadId": threadID,
		"input":    []map[string]string{{"type": "text", "text": message}},
	})
	if err != nil {
		return "", threadID, fmt.Errorf("start Codex turn: %w", err)
	}
	var turnInfo struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(turn["turn"], &turnInfo); err != nil || turnInfo.ID == "" {
		return "", threadID, errors.New("Codex returned no turn ID")
	}

	var response strings.Builder
	for {
		// Accumulate streamed text deltas until Codex signals that this turn has
		// finished, then return the complete text through the existing HTTP API.
		line, err := c.next(ctx)
		if err != nil {
			return "", threadID, err
		}
		var event struct {
			Method string `json:"method"`
			ID     any    `json:"id"`
			Params struct {
				TurnID string `json:"turnId"`
				Delta  string `json:"delta"`
				Turn   struct {
					ID     string `json:"id"`
					Status string `json:"status"`
				} `json:"turn"`
			} `json:"params"`
		}
		if err := json.Unmarshal(line, &event); err != nil {
			continue
		}
		switch event.Method {
		case "item/agentMessage/delta":
			if event.Params.TurnID == turnInfo.ID {
				response.WriteString(event.Params.Delta)
			}
		case "turn/completed":
			if event.Params.Turn.ID != turnInfo.ID {
				continue
			}
			if event.Params.Turn.Status != "completed" {
				return "", threadID, fmt.Errorf("Codex turn %s", event.Params.Turn.Status)
			}
			return response.String(), threadID, nil
		default:
			if event.ID != nil && event.Method != "" {
				// This endpoint has no approval UI, so reject server requests that
				// require a user decision rather than leaving the turn hanging.
				_ = c.respondError(event.ID, "This endpoint cannot approve Codex actions")
			}
		}
	}
}

// StartThread initializes app-server and creates a conversation without a user
// turn. ctx bounds startup and requests; developerInstructions seed the thread.
func (c *Client) StartThread(ctx context.Context, developerInstructions string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.start(ctx); err != nil {
		return "", err
	}
	if err := c.initialize(ctx); err != nil {
		return "", err
	}
	params := map[string]any{}
	if developerInstructions != "" {
		params["developerInstructions"] = developerInstructions
	}
	thread, err := c.request(ctx, "thread/start", params)
	if err != nil {
		return "", fmt.Errorf("start Codex thread: %w", err)
	}
	var info struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(thread["thread"], &info); err != nil || info.ID == "" {
		return "", errors.New("Codex returned no thread ID")
	}
	c.threads[info.ID] = struct{}{}
	return info.ID, nil
}

// initialize performs app-server's one-time client handshake. ctx bounds the
// JSON-RPC request; the Client lock must be held by the caller.
func (c *Client) initialize(ctx context.Context) error {
	if c.started {
		return nil
	}
	if _, err := c.request(ctx, "initialize", map[string]any{"clientInfo": map[string]string{
		"name": "company_backend", "title": "Company Backend", "version": "1.0.0",
	}}); err != nil {
		return fmt.Errorf("initialize Codex: %w", err)
	}
	if err := c.notify("initialized", map[string]any{}); err != nil {
		return err
	}
	c.started = true
	return nil
}

// isMissingThreadError recognizes app-server resume errors that mean the
// persisted thread is gone. err is the JSON-RPC error returned by app-server.
func isMissingThreadError(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "thread not found") || strings.Contains(message, "no rollout found")
}

// start launches app-server in WebSocket mode and retries the connection while
// the listener starts accepting connections. ctx limits how long startup may
// wait for that connection.
func (c *Client) start(ctx context.Context) error {
	if c.conn != nil {
		return nil
	}
	if c.external {
		return c.connect(ctx)
	}
	c.cmd = exec.Command(c.command, "app-server", "--listen", c.listenURL)
	configureCommandProcessGroup(c.cmd)
	// App-server uses stderr for its startup banner and diagnostics; protocol
	// traffic travels only over WebSocket, so its stdout is discarded.
	c.cmd.Stdout = io.Discard
	c.cmd.Stderr = os.Stderr
	if err := c.cmd.Start(); err != nil {
		c.cmd = nil
		return fmt.Errorf("start Codex app-server: %w", err)
	}
	c.processDone = make(chan struct{})
	// Wait separately so startup can detect an early process exit while trying
	// to connect, and shutdown can wait until the child has been reaped.
	go func() {
		c.processErr = c.cmd.Wait()
		close(c.processDone)
	}()

	return c.connect(ctx)
}

// connect opens the WebSocket to app-server and retries until it is ready.
// ctx bounds retries and listenURL is the app-server WebSocket endpoint.
func (c *Client) connect(ctx context.Context) error {
	dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var lastErr error
	for {
		// The process may need a short time to bind the port after Start returns.
		headers := http.Header{}
		if c.wsToken != "" {
			headers.Set("Authorization", "Bearer "+c.wsToken)
		}
		conn, _, err := dialer.DialContext(ctx, c.listenURL, headers)
		if err == nil {
			c.conn = conn
			return nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return fmt.Errorf("connect to Codex app-server at %s: %w", c.listenURL, ctx.Err())
		case <-c.processDone:
			if c.processDone != nil {
				return fmt.Errorf("Codex app-server exited before WebSocket connection: %v (last dial error: %v)", c.processErr, lastErr)
			}
		case <-ticker.C:
		}
	}
}

// request sends one JSON-RPC method call and waits for its matching reply. ctx
// bounds the wait, method is the app-server method name, and params is that
// method's JSON object. Unrelated notifications are queued for the turn loop.
func (c *Client) request(ctx context.Context, method string, params any) (map[string]json.RawMessage, error) {
	c.seq++
	id := c.seq
	if err := c.write(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	for {
		line, err := c.read(ctx)
		if err != nil {
			return nil, err
		}
		var msg struct {
			ID     *int                       `json:"id"`
			Method string                     `json:"method"`
			Result map[string]json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(line, &msg) != nil {
			continue
		}
		if msg.ID != nil && *msg.ID == id {
			if msg.Error != nil {
				return nil, errors.New(msg.Error.Message)
			}
			return msg.Result, nil
		}
		// App-server can send a request as well as notifications. Reject requests
		// that need a decision; save notifications for Send's turn event loop.
		if msg.ID != nil && msg.Method != "" {
			_ = c.respondError(msg.ID, "This endpoint cannot approve Codex actions")
		} else if msg.Method != "" {
			c.pending = append(c.pending, line)
		}
	}
}

// notify sends a JSON-RPC notification, which has no request ID or reply.
// method is the app-server notification name and params is its JSON payload.
func (c *Client) notify(method string, params any) error {
	return c.write(map[string]any{"method": method, "params": params})
}

// write sends one JSON object in a WebSocket text frame, the app-server's
// WebSocket message format. value is serialized as the frame's JSON payload.
func (c *Client) write(value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := c.conn.WriteMessage(websocket.TextMessage, data); err != nil {
		return fmt.Errorf("write to Codex app-server: %w", err)
	}
	return nil
}

// next returns the next queued notification or reads a new WebSocket message.
// ctx limits how long it waits for a message from app-server.
func (c *Client) next(ctx context.Context) ([]byte, error) {
	if len(c.pending) > 0 {
		line := c.pending[0]
		c.pending = c.pending[1:]
		return line, nil
	}
	return c.read(ctx)
}

// read applies the HTTP request deadline to the WebSocket read. AfterFunc also
// interrupts a blocked read immediately if the caller disconnects. ctx is the
// request context controlling the read's deadline and cancellation.
func (c *Client) read(ctx context.Context) ([]byte, error) {
	if deadline, ok := ctx.Deadline(); ok {
		_ = c.conn.SetReadDeadline(deadline)
	} else {
		_ = c.conn.SetReadDeadline(time.Time{})
	}
	stop := context.AfterFunc(ctx, func() { _ = c.conn.SetReadDeadline(time.Now()) })
	messageType, data, err := c.conn.ReadMessage()
	stop()
	_ = c.conn.SetReadDeadline(time.Time{})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("read from Codex app-server: %w", err)
	}
	if messageType != websocket.TextMessage {
		return nil, fmt.Errorf("unexpected Codex WebSocket message type %d", messageType)
	}
	return data, nil
}

// respondError rejects a JSON-RPC request made by app-server. id is the
// request's correlation ID and message explains why the backend rejected it.
func (c *Client) respondError(id any, message string) error {
	return c.write(map[string]any{"id": id, "error": map[string]string{"message": message}})
}

// Close stops the app-server process and closes the WebSocket during backend
// shutdown.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var closeErr error
	// Closing the WebSocket and terminating app-server releases every loaded
	// thread session. Avoid waiting for an unsubscribe response here because a
	// server that is already stopping may never send one.
	if c.conn != nil {
		if err := c.conn.Close(); err != nil {
			closeErr = errors.Join(closeErr, fmt.Errorf("close Codex WebSocket: %w", err))
		}
		c.conn = nil
	}
	c.threads = make(map[string]struct{})
	if c.cmd != nil && c.cmd.Process != nil {
		if err := terminateCommandProcessGroup(c.cmd); err != nil && !errors.Is(err, os.ErrProcessDone) {
			closeErr = errors.Join(closeErr, fmt.Errorf("terminate Codex app-server: %w", err))
		}
		select {
		case <-c.processDone:
			c.cmd = nil
		case <-time.After(5 * time.Second):
			closeErr = errors.Join(closeErr, errors.New("timed out waiting for Codex app-server to exit"))
		}
	}
	return closeErr
}
