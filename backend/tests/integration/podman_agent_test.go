package integration

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"company/backend/internal/codex"
)

// TestPodmanAgentContainerLifecycle starts an app-managed Podman agent,
// executes a shell command inside it, checks the result, and removes it. t
// provides isolated temporary agent data and reports lifecycle failures.
func TestPodmanAgentContainerLifecycle(t *testing.T) {
	podman := os.Getenv("PODMAN_BIN")
	if podman == "" {
		podman = "podman"
	}
	if _, err := exec.LookPath(podman); err != nil {
		t.Skipf("Podman is unavailable: %v", err)
	}
	image := os.Getenv("CODEX_PODMAN_IMAGE")
	if image == "" {
		image = "localhost/thecomp-codex:latest"
	}
	imageCtx, imageCancel := context.WithTimeout(context.Background(), 10*time.Second)
	imageCheck := exec.CommandContext(imageCtx, podman, "image", "exists", image)
	imageOutput, imageErr := imageCheck.CombinedOutput()
	imageCancel()
	if imageErr != nil {
		t.Skipf("Podman image %q is unavailable: %v (%s)", image, imageErr, strings.TrimSpace(string(imageOutput)))
	}
	authFile := os.Getenv("CODEX_AUTH_FILE")
	if authFile == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Skipf("cannot determine Codex home: %v", err)
		}
		authFile = filepath.Join(home, ".codex", "auth.json")
	}
	if _, err := os.Stat(authFile); err != nil {
		t.Skipf("local Codex login is unavailable: %v", err)
	}

	// Keep the agent's copied Codex credentials and thread state inside the
	// temporary test directory so cleanup does not affect the host Codex home.
	t.Setenv("CODEX_AGENT_DATA_DIR", t.TempDir())
	manager, err := codex.NewPodmanManager(image)
	if err != nil {
		t.Fatalf("create Podman manager: %v", err)
	}
	ownerID := time.Now().UnixNano() + 1_000_000_000_000_000
	const agentName = "tst"
	containerName := fmt.Sprintf("thecomp-u%d-%s", ownerID, agentName)
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := manager.RemoveAgent(cleanupCtx, ownerID, agentName); err != nil {
			t.Errorf("remove test agent container: %v", err)
			output, fallbackErr := exec.CommandContext(cleanupCtx, podman, "rm", "--force", containerName).CombinedOutput()
			if fallbackErr != nil && !strings.Contains(strings.ToLower(string(output)), "no such container") {
				t.Errorf("force-remove test container %s: %v (%s)", containerName, fallbackErr, strings.TrimSpace(string(output)))
			}
		}
		if err := manager.Close(); err != nil {
			t.Errorf("close Podman manager: %v", err)
		}
	})

	startCtx, startCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer startCancel()
	t.Log("starting the app-managed Codex container")
	if err := manager.EnsureAgent(startCtx, ownerID, agentName); err != nil {
		t.Fatalf("start app-managed agent: %v", err)
	}

	inspectCtx, inspectCancel := context.WithTimeout(context.Background(), 10*time.Second)
	inspect := exec.CommandContext(inspectCtx, podman, "inspect", "--format", "{{.State.Running}}", containerName)
	inspectOutput, inspectErr := inspect.CombinedOutput()
	inspectCancel()
	if inspectErr != nil || strings.TrimSpace(string(inspectOutput)) != "true" {
		t.Fatalf("container %s is not running: %v (%s)", containerName, inspectErr, strings.TrimSpace(string(inspectOutput)))
	}

	// Write and read a random marker in the container to prove podman exec can
	// run commands in the live agent environment and return their output.
	var randomMarker [12]byte
	if _, err := rand.Read(randomMarker[:]); err != nil {
		t.Fatalf("create unique command marker: %v", err)
	}
	marker := hex.EncodeToString(randomMarker[:])
	markerPath := "/tmp/thecomp-integration-" + marker
	command := fmt.Sprintf("printf '%%s' '%s' > '%s' && cat '%s'", marker, markerPath, markerPath)
	commandCtx, commandCancel := context.WithTimeout(context.Background(), 15*time.Second)
	containerCommand := exec.CommandContext(commandCtx, podman, "exec", containerName, "/bin/sh", "-c", command)
	commandOutput, commandErr := containerCommand.CombinedOutput()
	commandCancel()
	if commandErr != nil {
		t.Fatalf("run shell command in %s: %v (%s)", containerName, commandErr, strings.TrimSpace(string(commandOutput)))
	}
	if strings.TrimSpace(string(commandOutput)) != marker {
		t.Fatalf("container command returned %q, want marker %q", strings.TrimSpace(string(commandOutput)), marker)
	}

	listCtx, listCancel := context.WithTimeout(context.Background(), 10*time.Second)
	containers, err := manager.ListAgents(listCtx, ownerID, false)
	listCancel()
	if err != nil {
		t.Fatalf("list owner's agent containers: %v", err)
	}
	if len(containers) != 1 || containers[0].Name != containerName {
		t.Fatalf("listed containers = %+v, want only %s", containers, containerName)
	}

	removeCtx, removeCancel := context.WithTimeout(context.Background(), 20*time.Second)
	if err := manager.RemoveAgent(removeCtx, ownerID, agentName); err != nil {
		removeCancel()
		t.Fatalf("remove app-managed container: %v", err)
	}
	removeCancel()

	existsCtx, existsCancel := context.WithTimeout(context.Background(), 10*time.Second)
	exists := exec.CommandContext(existsCtx, podman, "container", "exists", containerName)
	existsErr := exists.Run()
	existsCancel()
	if exitErr, ok := existsErr.(*exec.ExitError); !ok || exitErr.ExitCode() != 1 {
		t.Fatalf("container %s still exists after removal (error: %v)", containerName, existsErr)
	}
}
