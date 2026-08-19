package cli

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/herdanis/his-mouse-friday/internal/config"
	"github.com/herdanis/his-mouse-friday/internal/daemon"
	"github.com/herdanis/his-mouse-friday/internal/protocol"
)

// spinCLIDaemon sets up a short temp HMF_STATE_DIR (macOS socket-path limit),
// starts a daemon with /bin/echo launcher, registers a workspace + project,
// posts a task thread-root, returns the thread root id + general channel id +
// cleanup. The CLI's callDaemon + doneCmd both reach this temp daemon via
// protocol.SocketPath() (which respects HMF_STATE_DIR).
func spinCLIDaemon(t *testing.T) (threadRootID int64, generalChannelID int64, cleanup func()) {
	t.Helper()
	stateDir := filepath.Join("/tmp", "hmf-cli-"+t.Name())
	os.RemoveAll(stateDir)
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
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

	ctx, cancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	go func() { serveErr <- d.Serve(ctx) }()

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

	// Register a workspace + inbound-allowed project.
	userDir := t.TempDir()
	os.WriteFile(filepath.Join(userDir, "mouse.yaml"),
		[]byte("agent:\n  primary:\n    provider: /bin/echo\na2a:\n  allow_inbound: true\n"), 0644)
	mustCLICall(t, "workspace_add", map[string]any{"name": "companyA"})
	mustCLICall(t, "project_add", map[string]any{"workspace": "companyA", "name": "user-service", "path": userDir})

	// Post a task thread-root (wakes /bin/echo, which exits; that's fine — we
	// just need the thread root id + general channel id for the done reply).
	postResult := mustCLICall(t, "post_message", map[string]any{
		"from":    "companyA/payment",
		"to":      "companyA/user-service",
		"content": "task",
	})
	var pr daemon.PostResult
	json.Unmarshal(postResult, &pr)

	// Look up the general channel id (the task was posted there).
	genResult := mustCLICall(t, "read_channel", map[string]any{})
	var msgs []daemon.Message
	json.Unmarshal(genResult, &msgs)
	genID := msgs[0].ChannelID

	cleanup = func() {
		cancel()
		select {
		case <-serveErr:
		case <-time.After(2 * time.Second):
		}
		d.Store.Close()
		os.RemoveAll(stateDir)
	}
	return pr.MessageID, genID, cleanup
}

func mustCLICall(t *testing.T, method string, params any) json.RawMessage {
	t.Helper()
	result, err := protocol.Call(method, params)
	if err != nil {
		t.Fatalf("Call %s: %v", method, err)
	}
	return result
}

// hmf done posts a threaded done reply using the spawned-agent env vars.
// Guards the agent's reply path (the alternative to the post_message MCP tool).
func TestCLI_DonePostsThreadedReply(t *testing.T) {
	threadRootID, generalChannelID, cleanup := spinCLIDaemon(t)
	defer cleanup()

	// Simulate the spawned agent's env (set by the launcher).
	t.Setenv("HMF_CHANNEL_ID", itoa(generalChannelID))
	t.Setenv("HMF_TASK_MSG_ID", itoa(threadRootID))
	t.Setenv("HMF_PROJECT", "companyA/user-service")
	t.Setenv("HMF_FROM", "companyA/payment")

	// Invoke `hmf done "all done"`.
	root := NewRootCmd()
	root.SetArgs([]string{"done", "all", "done"})
	if err := root.Execute(); err != nil {
		t.Fatalf("done cmd: %v", err)
	}

	// Verify the done reply landed: status=done, thread_id=threadRootID,
	// content="all done", from=HMF_PROJECT, to=HMF_FROM.
	result := mustCLICall(t, "read_thread", map[string]any{"thread_id": threadRootID})
	var msgs []daemon.Message
	json.Unmarshal(result, &msgs)
	// root (task) + done reply.
	if len(msgs) < 2 {
		t.Fatalf("got %d messages, want >=2 (root + done)", len(msgs))
	}
	done := msgs[len(msgs)-1]
	if done.Status != "done" {
		t.Errorf("status: got %q want done", done.Status)
	}
	if done.Content != "all done" {
		t.Errorf("content: got %q want 'all done'", done.Content)
	}
	if done.ThreadID != threadRootID {
		t.Errorf("thread_id: got %d want %d", done.ThreadID, threadRootID)
	}
	if done.FromProject != "companyA/user-service" || done.ToProject != "companyA/payment" {
		t.Errorf("from/to: got %q/%q want user-service/payment", done.FromProject, done.ToProject)
	}
}

// hmf done errors when HMF_CHANNEL_ID is not set (not a spawned agent).
func TestCLI_DoneRequiresChannelID(t *testing.T) {
	_, _, cleanup := spinCLIDaemon(t)
	defer cleanup()

	// No HMF_CHANNEL_ID set.
	os.Unsetenv("HMF_CHANNEL_ID")
	root := NewRootCmd()
	root.SetArgs([]string{"done", "x"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error when HMF_CHANNEL_ID is not set")
	}
}

// hmf done still posts (unthreaded) when HMF_TASK_MSG_ID is missing — the
// agent can signal completion without a thread root (rare, but shouldn't error).
func TestCLI_DoneWithoutTaskMsgID(t *testing.T) {
	_, generalChannelID, cleanup := spinCLIDaemon(t)
	defer cleanup()

	t.Setenv("HMF_CHANNEL_ID", itoa(generalChannelID))
	os.Unsetenv("HMF_TASK_MSG_ID") // no thread
	t.Setenv("HMF_PROJECT", "companyA/user-service")
	t.Setenv("HMF_FROM", "companyA/payment")

	root := NewRootCmd()
	root.SetArgs([]string{"done", "standalone"})
	if err := root.Execute(); err != nil {
		t.Fatalf("done cmd: %v", err)
	}

	// Verify a done message landed (unthreaded — thread_id=0).
	result := mustCLICall(t, "read_channel", map[string]any{})
	var msgs []daemon.Message
	json.Unmarshal(result, &msgs)
	var found bool
	for _, m := range msgs {
		if m.Status == "done" && m.Content == "standalone" && m.ThreadID == 0 {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("unthreaded done not found in: %+v", msgs)
	}
}

// TestCLI_SessionList verifies the session_list daemon RPC returns at least
// one session row (spinCLIDaemon already posted a wake task → 1 session)
// with Name/Project/Status populated (prefix + name set by Task 10).
func TestCLI_SessionList(t *testing.T) {
	threadRootID, generalChannelID, cleanup := spinCLIDaemon(t)
	defer cleanup()

	result := mustCLICall(t, "session_list", map[string]any{})
	var items []daemon.SessionListItem
	if err := json.Unmarshal(result, &items); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(items) == 0 {
		t.Fatal("expected at least 1 session, got 0")
	}
	first := items[0]
	if first.Name == "" || first.Status == "" || first.Project == "" {
		t.Errorf("missing fields: %+v", first)
	}
	_ = threadRootID
	_ = generalChannelID
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
