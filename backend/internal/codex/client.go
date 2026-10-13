package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
)

// Client owns one persistent Codex app-server process and its newline-delimited
// JSON-RPC connection. A single app-server can hold many conversation threads;
// Send serializes turns so each caller receives only its own response events.
type Client struct {
	mu      sync.Mutex
	command string
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	lines   chan []byte
	readErr chan error
	seq     int
	started bool
}

func NewClient(command string) *Client {
	// Allow deployments to point at a nonstandard Codex executable, while keeping
	// the normal CLI name as the default.
	if command == "" {
		command = "codex"
	}
	return &Client{command: command}
}

// Send submits one user message and waits for the completed agent response.
// An empty threadID starts a new conversation; a provided ID resumes that
// conversation so Codex can use its earlier turns as context. The returned ID
// must be stored by the caller and sent with the next message in that chat.
func (c *Client) Send(ctx context.Context, message, threadID string) (string, string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.start(); err != nil {
		return "", "", err
	}
	if !c.started {
		// The app-server protocol requires one initialize handshake per process.
		// The initialized notification confirms that the client is ready to issue
		// normal methods such as thread/start and turn/start.
		if _, err := c.request(ctx, "initialize", map[string]any{"clientInfo": map[string]string{
			"name": "company_backend", "title": "Company Backend", "version": "1.0.0",
		}}); err != nil {
			return "", "", fmt.Errorf("initialize Codex: %w", err)
		}
		if err := c.notify("initialized", map[string]any{}); err != nil {
			return "", "", err
		}
		c.started = true
	}

	var thread map[string]json.RawMessage
	var err error
	// Each message belongs to a thread. Starting one creates a new, unrelated
	// conversation; resuming requires a valid thread ID returned by an earlier
	// call to this endpoint.
	if threadID == "" {
		thread, err = c.request(ctx, "thread/start", map[string]any{})
	} else {
		thread, err = c.request(ctx, "thread/resume", map[string]any{"threadId": threadID})
	}
	if err != nil {
		return "", "", fmt.Errorf("start Codex thread: %w", err)
	}
	if threadID == "" {
		// thread/start wraps its identifier in a `thread` object. Extract it so
		// the HTTP API can return it to the caller for future requests.
		var info struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(thread["thread"], &info); err != nil || info.ID == "" {
			return "", "", errors.New("Codex returned no thread ID")
		}
		threadID = info.ID
	}

	// A thread is the conversation container; a turn is one user message and
	// Codex's response to it. `input` is an array to match app-server's protocol.
	if _, err := c.request(ctx, "turn/start", map[string]any{
		"threadId": threadID,
		"input":    []map[string]string{{"type": "text", "text": message}},
	}); err != nil {
		return "", threadID, fmt.Errorf("start Codex turn: %w", err)
	}
	var response strings.Builder
	for {
		// Agent text arrives as incremental delta events. Accumulate it here
		// because this HTTP endpoint returns a single JSON response after the turn.
		line, err := c.next(ctx)
		if err != nil {
			return "", threadID, err
		}
		var event struct {
			Method string `json:"method"`
			ID     any    `json:"id"`
			Params struct {
				Delta string `json:"delta"`
				Turn  struct {
					Status string `json:"status"`
				} `json:"turn"`
			} `json:"params"`
		}
		if err := json.Unmarshal(line, &event); err != nil {
			continue
		}
		switch event.Method {
		case "item/agentMessage/delta":
			response.WriteString(event.Params.Delta)
		case "turn/completed":
			// A completion event can also describe a failed or interrupted turn.
			// Only report success when Codex marks the turn completed.
			if event.Params.Turn.Status != "completed" {
				return "", threadID, fmt.Errorf("Codex turn %s", event.Params.Turn.Status)
			}
			return response.String(), threadID, nil
		default:
			if event.ID != nil && event.Method != "" {
				// App-server can ask the client to approve a command or other action.
				// This synchronous endpoint has no approval UI, so reject the request
				// instead of leaving Codex blocked while waiting for a decision.
				_ = c.respondError(event.ID, "This endpoint cannot approve Codex actions")
			}
		}
	}
}

func (c *Client) start() error {
	if c.cmd != nil {
		return nil
	}
	// The app-server outlives an individual HTTP request. Close() owns its
	// lifetime; binding it to the first request would kill it after that reply.
	// stdio:// makes the process communicate over the pipes below. Keep the
	// process alive between HTTP calls to preserve its initialized state.
	c.cmd = exec.Command(c.command, "app-server", "--listen", "stdio://")
	stdin, err := c.cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("open Codex stdin: %w", err)
	}
	stdout, err := c.cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("open Codex stdout: %w", err)
	}
	if err := c.cmd.Start(); err != nil {
		c.cmd = nil
		return fmt.Errorf("start Codex app-server: %w", err)
	}
	c.stdin, c.lines, c.readErr = stdin, make(chan []byte, 32), make(chan error, 1)
	go func() {
		// JSON-RPC messages are newline-delimited. Copy each scanner buffer before
		// the scanner reuses it, then hand messages to the request goroutine.
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 4096), 16<<20)
		for scanner.Scan() {
			line := append([]byte(nil), scanner.Bytes()...)
			c.lines <- line
		}
		if err := scanner.Err(); err != nil {
			c.readErr <- err
		} else {
			c.readErr <- io.EOF
		}
	}()
	return nil
}

func (c *Client) request(ctx context.Context, method string, params any) (map[string]json.RawMessage, error) {
	c.seq++
	id := c.seq
	// Numeric request IDs let this client match a reply to the method call that
	// sent it, even when app-server notifications appear between replies.
	if err := c.write(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	for {
		line, err := c.next(ctx)
		if err != nil {
			return nil, err
		}
		var msg struct {
			ID     *int                       `json:"id"`
			Result map[string]json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(line, &msg) != nil || msg.ID == nil || *msg.ID != id {
			continue
		}
		if msg.Error != nil {
			return nil, errors.New(msg.Error.Message)
		}
		return msg.Result, nil
	}
}

func (c *Client) notify(method string, params any) error {
	// Notifications have no request ID and therefore do not expect a reply.
	return c.write(map[string]any{"method": method, "params": params})
}

func (c *Client) write(value any) error {
	// One JSON object per line is the app-server stdio framing format.
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if _, err := c.stdin.Write(data); err != nil {
		return fmt.Errorf("write to Codex app-server: %w", err)
	}
	return nil
}

func (c *Client) next(ctx context.Context) ([]byte, error) {
	// Respect the HTTP request deadline while waiting for app-server output.
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case err := <-c.readErr:
		return nil, fmt.Errorf("read Codex app-server: %w", err)
	case line := <-c.lines:
		return line, nil
	}
}

func (c *Client) respondError(id any, message string) error {
	return c.write(map[string]any{"id": id, "error": map[string]string{"message": message}})
}

func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stdin != nil {
		_ = c.stdin.Close()
	}
	if c.cmd != nil && c.cmd.Process != nil {
		// The process is intentionally persistent during normal operation; stop it
		// when the backend is shutting down.
		_ = c.cmd.Process.Kill()
		return c.cmd.Wait()
	}
	return nil
}
