package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/herdanis/his-mouse-friday/internal/config"
	"github.com/herdanis/his-mouse-friday/internal/daemon"
	"github.com/herdanis/his-mouse-friday/internal/protocol"
)

// These tests guard the MCP shim's CONTRACT with the daemon: the param shapes
// each tool sends are accepted by the daemon, and the daemon's responses
// unmarshal cleanly into the shim's output types (PostOutput.message_id,
// TaskStatusOutput.agent_status, []MessageOutput). A refactor that breaks the
// message_id return, task_status shape, or read_thread/read_channel defaults
// fails here before it ships.

// spinDaemon sets up a temp HMF_STATE_DIR, starts a daemon with /bin/echo
// launcher, registers a workspace + an inbound-allowed project, returns the
// daemon + a cleanup. The shim's callDaemon dials protocol.SocketPath() which
// resolves under HMF_STATE_DIR, so the shim reaches this temp daemon.
func spinDaemon(t *testing.T) (*daemon.Daemon, func()) {
	t.Helper()
	// macOS limits unix socket paths to ~104 chars; t.TempDir() paths are too
	// long. Use a short dir under /tmp with a unique suffix.
	stateDir := filepath.Join("/tmp", fmt.Sprintf("hmf-shim-%d", time.Now().UnixNano()))
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	t.Setenv("HMF_STATE_DIR", stateDir)
	sock := filepath.Join(stateDir, "daemon.sock")
	dbPath := filepath.Join(stateDir, "hmf.db")

	d, err := daemon.NewDaemon(sock, dbPath)
	if err != nil {
		t.Fatalf("NewDaemon: %v", err)
	}
	d.Launcher = &daemon.Launcher{Binary: "/bin/echo"}
	d.MouseLoader = config.LoadMouse
	// /bin/echo exits immediately without posting a done reply — the safety
	// net would post synthetic BLOCKED replies and pollute the threads these
	// tests assert on. Disable it (matches setupDaemon in daemon_test.go).
	d.SafetyNetEnabled = false

	ctx, cancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	go func() { serveErr <- d.Serve(ctx) }()

	// Wait for socket, or fail fast if Serve returned an error.
	deadline, cancelWait := context.WithTimeout(ctx, 3*time.Second)
	for {
		select {
		case err := <-serveErr:
			cancelWait()
			t.Fatalf("Serve returned early: %v", err)
		default:
		}
		if c, err := net.Dial("unix", sock); err == nil {
			c.Close()
			break
		}
		if deadline.Err() != nil {
			cancelWait()
			t.Fatalf("socket never accepted")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancelWait()

	// Register a workspace + an inbound-allowed project (mouse.yaml in its dir).
	userDir := t.TempDir()
	os.WriteFile(filepath.Join(userDir, "mouse.yaml"),
		[]byte("agent:\n  primary:\n    provider: /bin/echo\na2a:\n  allow_inbound: true\n"), 0644)
	mustCallDaemon(t, "workspace_add", map[string]any{"name": "companyA"})
	mustCallDaemon(t, "project_add", map[string]any{"workspace": "companyA", "name": "user-service", "path": userDir})

	cleanup := func() {
		cancel()
		select {
		case <-serveErr:
		case <-time.After(2 * time.Second):
		}
		d.Store.Close()
		os.RemoveAll(stateDir)
	}
	return d, cleanup
}

// mustCallDaemon is a test helper that calls protocol.Call + fatals on error.
// protocol.Call returns (result, error) — it folds daemon errors into the error.
func mustCallDaemon(t *testing.T, method string, params any) json.RawMessage {
	t.Helper()
	result, err := protocol.Call(method, params)
	if err != nil {
		t.Fatalf("Call %s: %v", method, err)
	}
	return result
}

// post_message with a `to` returns {message_id} + wakes the agent. The
// orchestrator relies on message_id to poll task_status + read_thread.
func TestShim_PostMessageReturnsMessageID(t *testing.T) {
	_, cleanup := spinDaemon(t)
	defer cleanup()

	result := mustCallDaemon(t, "post_message", map[string]any{
		"from":    "companyA/payment",
		"to":      "companyA/user-service",
		"content": "do the thing",
	})
	var out PostOutput
	if err := json.Unmarshal(result, &out); err != nil {
		t.Fatalf("unmarshal PostOutput: %v", err)
	}
	if out.MessageID == 0 {
		t.Fatal("post_message did not return a message_id — orchestrator can't poll task_status")
	}
}

// task_status returns the agent_status + has_done shape the orchestrator polls.
func TestShim_TaskStatusShape(t *testing.T) {
	_, cleanup := spinDaemon(t)
	defer cleanup()

	// Post a task — wakes /bin/echo (exits immediately → session marked exited).
	postResult := mustCallDaemon(t, "post_message", map[string]any{
		"from":    "companyA/payment",
		"to":      "companyA/user-service",
		"content": "x",
	})
	var pr PostOutput
	json.Unmarshal(postResult, &pr)
	threadID := pr.MessageID

	// Poll task_status — must unmarshal into TaskStatusOutput cleanly.
	result := mustCallDaemon(t, "task_status", map[string]any{"message_id": threadID})
	var ts TaskStatusOutput
	if err := json.Unmarshal(result, &ts); err != nil {
		t.Fatalf("unmarshal TaskStatusOutput: %v", err)
	}
	// /bin/echo exits fast — expect exited or working, has_done false (no done reply).
	if ts.AgentStatus != "exited" && ts.AgentStatus != "working" {
		t.Errorf("agent_status: got %q, want exited|working", ts.AgentStatus)
	}
	if ts.HasDone {
		t.Error("has_done: got true, want false (no done reply was posted)")
	}
}

// read_thread returns []MessageOutput — root + replies. Guards the orchestrator's
// collect path.
func TestShim_ReadThreadShape(t *testing.T) {
	_, cleanup := spinDaemon(t)
	defer cleanup()

	postResult := mustCallDaemon(t, "post_message", map[string]any{
		"from":    "companyA/payment",
		"to":      "companyA/user-service",
		"content": "task",
	})
	var pr PostOutput
	json.Unmarshal(postResult, &pr)
	threadID := pr.MessageID

	// Post a reply in-thread.
	mustCallDaemon(t, "post_message", map[string]any{
		"thread_id": threadID,
		"from":      "companyA/user-service",
		"to":        "companyA/payment",
		"content":   "done",
		"status":    "done",
	})

	result := mustCallDaemon(t, "read_thread", map[string]any{"thread_id": threadID})
	var msgs []MessageOutput
	if err := json.Unmarshal(result, &msgs); err != nil {
		t.Fatalf("unmarshal []MessageOutput: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2 (root + done)", len(msgs))
	}
	if msgs[0].Content != "task" || msgs[1].Content != "done" || msgs[1].Status != "done" {
		t.Errorf("messages: got %+v", msgs)
	}
	if msgs[1].ThreadID != threadID {
		t.Errorf("reply thread_id: got %d want %d", msgs[1].ThreadID, threadID)
	}
}

// read_channel with no channel defaults to the global general channel.
// Guards the orchestrator's "see lobby messages" path.
func TestShim_ReadChannelDefaultsToGeneral(t *testing.T) {
	_, cleanup := spinDaemon(t)
	defer cleanup()

	// Post a message (defaults to general since no channel given).
	mustCallDaemon(t, "post_message", map[string]any{
		"from":    "companyA/payment",
		"to":      "companyA/user-service",
		"content": "lobby post",
	})

	// read_channel with no channel — should return general's messages.
	result := mustCallDaemon(t, "read_channel", map[string]any{})
	var msgs []MessageOutput
	if err := json.Unmarshal(result, &msgs); err != nil {
		t.Fatalf("unmarshal []MessageOutput: %v", err)
	}
	if len(msgs) == 0 {
		t.Fatal("read_channel (no channel) returned no messages — general default broken")
	}
	if msgs[len(msgs)-1].Content != "lobby post" {
		t.Errorf("latest: got %q want 'lobby post'", msgs[len(msgs)-1].Content)
	}
}
