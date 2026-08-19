package daemon

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/herdanis/his-mouse-friday/internal/config"
	"github.com/herdanis/his-mouse-friday/internal/protocol"
)

// awaitSockTimeout caps how long TestServe_* waits for the listener to appear.
const awaitSockTimeout = 2 * time.Second

func setupDaemon(t *testing.T) *Daemon {
	t.Helper()
	store := newTestStore(t)
	dir := t.TempDir()
	_ = dir
	return &Daemon{
		Store:    store,
		Registry: &Registry{Store: store},
		Sessions: &SessionStore{Store: store},
		Comms:    &Comms{Store: store},
		Launcher: &Launcher{Binary: "/bin/echo"},
		MouseLoader: func(path string) (*config.MouseConfig, error) {
			return config.LoadMouse(path)
		},
		shutdownCh: make(chan struct{}),
	}
}

func TestHandle_PostToGeneralWakesAgent(t *testing.T) {
	d := setupDaemon(t)
	d.Registry.AddWorkspace("companyA")
	userDir := t.TempDir()
	os.WriteFile(filepath.Join(userDir, "mouse.yaml"), []byte("agent:\n  primary:\n    provider: opencode\na2a:\n  allow_inbound: true\n"), 0644)
	d.Registry.AddProject("companyA", "user-service", userDir)

	// Post a task to general mentioning @companyA/user-service — thread root.
	params, _ := json.Marshal(map[string]any{
		"from":    "companyA/payment-service",
		"to":      "companyA/user-service",
		"content": "add field payment_status",
	})
	resp := d.Handle(context.Background(), protocol.Request{Method: "post_message", Params: params, ID: 1})
	if resp.Error != nil {
		t.Fatalf("post failed: %s", resp.Error.Message)
	}
	var pr PostResult
	json.Unmarshal(resp.Result, &pr)
	if pr.MessageID == 0 {
		t.Fatal("no message id")
	}
	// Wake fired: a session was created for the addressed project.
	var sessCount int
	d.Store.db.QueryRow(
		`SELECT count(*) FROM sessions WHERE project_id=
		  (SELECT id FROM projects WHERE name='user-service')`).Scan(&sessCount)
	if sessCount == 0 {
		t.Fatal("post to general did not wake the addressed agent (no session created)")
	}
}

// TestHandle_SyntheticBlockedReplyOnSilentExit verifies the daemon-side safety
// net: when a spawned agent exits without posting a done reply (e.g. hit
// bash:ask with no TTY), the daemon posts a synthetic BLOCKED reply on its
// behalf so the orchestrator stops polling. setupDaemon uses /bin/echo as the
// agent binary, which exits immediately without posting — perfect simulacrum
// of the silent-exit failure mode.
func TestHandle_SyntheticBlockedReplyOnSilentExit(t *testing.T) {
	d := setupDaemon(t)
	d.Registry.AddWorkspace("companyA")
	userDir := t.TempDir()
	os.WriteFile(filepath.Join(userDir, "mouse.yaml"), []byte("agent:\n  primary:\n    provider: opencode\na2a:\n  allow_inbound: true\n"), 0644)
	d.Registry.AddProject("companyA", "user-service", userDir)

	params, _ := json.Marshal(map[string]any{
		"from":    "companyA/payment-service",
		"to":      "companyA/user-service",
		"content": "do something with bash",
	})
	resp := d.Handle(context.Background(), protocol.Request{Method: "post_message", Params: params, ID: 1})
	if resp.Error != nil {
		t.Fatalf("post failed: %s", resp.Error.Message)
	}
	var pr PostResult
	json.Unmarshal(resp.Result, &pr)

	// /bin/echo exits near-instantly. Wait for OnExit to fire + the synthetic
	// reply to land. Poll up to 2s.
	deadline := time.Now().Add(2 * time.Second)
	var gotBlocked bool
	for time.Now().Before(deadline) {
		var content, status string
		err := d.Store.db.QueryRow(
			`SELECT content, status FROM messages WHERE thread_id=? AND status='done'`, pr.MessageID).
			Scan(&content, &status)
		if err == nil && strings.HasPrefix(content, "BLOCKED:") {
			gotBlocked = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !gotBlocked {
		t.Fatal("no synthetic BLOCKED reply posted after silent agent exit")
	}
}

func TestHandle_Post_InboundDenied(t *testing.T) {
	d := setupDaemon(t)
	d.Registry.AddWorkspace("companyA")
	userDir := t.TempDir()
	os.WriteFile(filepath.Join(userDir, "mouse.yaml"), []byte("agent:\n  primary:\n    provider: opencode\n"), 0644)
	d.Registry.AddProject("companyA", "user-service", userDir)

	params, _ := json.Marshal(map[string]any{"from": "companyA/payment", "to": "companyA/user-service", "content": "x"})
	resp := d.Handle(context.Background(), protocol.Request{Method: "post_message", Params: params, ID: 1})
	if resp.Error == nil {
		t.Fatal("expected inbound-denied error")
	}
}

func TestHandle_PostAndRead(t *testing.T) {
	d := setupDaemon(t)
	d.Store.db.Exec(`INSERT INTO workspaces(id, name) VALUES(1, 'companyA')`)
	d.Store.db.Exec(`INSERT INTO channels(id, workspace_id, name, type) VALUES(10, 1, 'dm', 'dm')`)

	postParams, _ := json.Marshal(map[string]any{
		"channel": 10, "from": "companyA/payment", "to": "companyA/user", "content": "hello",
	})
	resp := d.Handle(context.Background(), protocol.Request{Method: "post_message", Params: postParams, ID: 1})
	if resp.Error != nil {
		t.Fatal(resp.Error.Message)
	}

	readParams, _ := json.Marshal(map[string]any{"channel": 10})
	resp = d.Handle(context.Background(), protocol.Request{Method: "read_channel", Params: readParams, ID: 2})
	if resp.Error != nil {
		t.Fatal(resp.Error.Message)
	}
	var msgs []Message
	json.Unmarshal(resp.Result, &msgs)
	if len(msgs) != 1 || msgs[0].Content != "hello" || msgs[0].FromProject != "companyA/payment" {
		t.Errorf("got %+v", msgs)
	}
}

func TestHandle_PostToUnregisteredAgentSkipsWake(t *testing.T) {
	d := setupDaemon(t)
	d.Registry.AddWorkspace("companyA")

	// Addressing an unregistered agent: message posts, no wake (mailbox semantics).
	params, _ := json.Marshal(map[string]any{"from": "companyA/payment", "to": "companyA/ghost", "content": "hello"})
	resp := d.Handle(context.Background(), protocol.Request{Method: "post_message", Params: params, ID: 1})
	if resp.Error != nil {
		t.Fatalf("post to unregistered agent should succeed (skip wake): %s", resp.Error.Message)
	}
	var sessCount int
	d.Store.db.QueryRow("SELECT count(*) FROM sessions").Scan(&sessCount)
	if sessCount != 0 {
		t.Fatalf("no wake expected for unregistered agent, got %d sessions", sessCount)
	}
}

// ============================================
// task_status — guards the orchestrator's "still working vs died vs done" signal
// ============================================

// postTask sets up a thread-root message + (optionally) a session linked by
// task_msg_id, returning the thread root id. Decouples task_status logic from
// spawn timing — we insert rows directly.
func postTask(t *testing.T, d *Daemon, sessionStatus string, exitCode int, withDone bool) int64 {
	t.Helper()
	d.Store.db.Exec(`INSERT INTO workspaces(id, name) VALUES(1, 'companyA')`)
	d.Store.db.Exec(`INSERT INTO projects(id, workspace_id, name, path) VALUES(1, 1, 'user-service', '/tmp/user')`)

	// Thread root message in the general channel.
	res, err := d.Store.db.Exec(
		`INSERT INTO messages(channel_id, thread_id, from_project, to_project, content, status, ts)
		 VALUES(1, NULL, 'companyA/payment', 'companyA/user-service', 'task', 'message', datetime('now'))`)
	if err != nil {
		t.Fatal(err)
	}
	rootID, _ := res.LastInsertId()

	if sessionStatus != "" {
		d.Store.db.Exec(
			`INSERT INTO sessions(project_id, agent_binary, model, status, pid, created_at, task_msg_id, exit_code)
			 VALUES(1, 'opencode', 'default', ?, 12345, datetime('now'), ?, ?)`,
			sessionStatus, rootID, exitCode)
	}
	if withDone {
		d.Store.db.Exec(
			`INSERT INTO messages(channel_id, thread_id, from_project, to_project, content, status, ts)
			 VALUES(1, ?, 'companyA/user-service', 'companyA/payment', 'done', 'done', datetime('now'))`,
			rootID)
	}
	return rootID
}

func TestHandle_TaskStatus_Working(t *testing.T) {
	d := setupDaemon(t)
	rootID := postTask(t, d, "active", 0, false)
	resp := d.Handle(context.Background(), protocol.Request{
		Method: "task_status",
		Params: mustJSON(t, map[string]any{"thread_id": rootID}),
		ID:     1,
	})
	if resp.Error != nil {
		t.Fatal(resp.Error.Message)
	}
	var ts TaskStatusResult
	json.Unmarshal(resp.Result, &ts)
	if ts.AgentStatus != "working" || ts.HasDone {
		t.Fatalf("want working/no-done, got %+v", ts)
	}
}

func TestHandle_TaskStatus_ExitedWithDone(t *testing.T) {
	d := setupDaemon(t)
	rootID := postTask(t, d, "exited", 0, true)
	resp := d.Handle(context.Background(), protocol.Request{
		Method: "task_status",
		Params: mustJSON(t, map[string]any{"thread_id": rootID}),
		ID:     1,
	})
	var ts TaskStatusResult
	json.Unmarshal(resp.Result, &ts)
	if ts.AgentStatus != "exited" || !ts.HasDone || ts.ExitCode != 0 {
		t.Fatalf("want exited+done+code0, got %+v", ts)
	}
}

func TestHandle_TaskStatus_FailedNoDone(t *testing.T) {
	d := setupDaemon(t)
	rootID := postTask(t, d, "failed", 1, false)
	resp := d.Handle(context.Background(), protocol.Request{
		Method: "task_status",
		Params: mustJSON(t, map[string]any{"thread_id": rootID}),
		ID:     1,
	})
	var ts TaskStatusResult
	json.Unmarshal(resp.Result, &ts)
	if ts.AgentStatus != "failed" || ts.HasDone || ts.ExitCode != 1 {
		t.Fatalf("want failed+no-done+code1, got %+v", ts)
	}
}

func TestHandle_TaskStatus_NoAgent(t *testing.T) {
	d := setupDaemon(t)
	// No session linked to the thread root — wake never fired.
	rootID := postTask(t, d, "", 0, false)
	resp := d.Handle(context.Background(), protocol.Request{
		Method: "task_status",
		Params: mustJSON(t, map[string]any{"thread_id": rootID}),
		ID:     1,
	})
	var ts TaskStatusResult
	json.Unmarshal(resp.Result, &ts)
	if ts.AgentStatus != "no_agent" || ts.HasDone {
		t.Fatalf("want no_agent/no-done, got %+v", ts)
	}
}

func TestHandle_TaskStatus_RequiresThreadID(t *testing.T) {
	d := setupDaemon(t)
	resp := d.Handle(context.Background(), protocol.Request{
		Method: "task_status",
		Params: mustJSON(t, map[string]any{}),
		ID:     1,
	})
	if resp.Error == nil {
		t.Fatal("expected error for missing thread_id")
	}
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, _ := json.Marshal(v)
	return b
}

// TestServe_Smoke exercises the unix socket server end-to-end:
// start Serve, send one status request over the socket, read the response.
func TestServe_Smoke(t *testing.T) {
	d := setupDaemon(t)
	sock := filepath.Join(t.TempDir(), "daemon.sock")
	d.Sock = sock

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveErr := make(chan error, 1)
	go func() { serveErr <- d.Serve(ctx) }()

	// Wait for socket to accept connections (retry dial until ready).
	var conn net.Conn
	var err error
	deadline, cancelWait := context.WithTimeout(ctx, awaitSockTimeout)
	for {
		conn, err = net.Dial("unix", sock)
		if err == nil {
			break
		}
		if deadline.Err() != nil {
			cancelWait()
			t.Fatalf("socket never accepted: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancelWait()
	defer conn.Close()

	enc := json.NewEncoder(conn)
	dec := json.NewDecoder(conn)
	req := protocol.Request{Method: "status", ID: 42}
	if err := enc.Encode(&req); err != nil {
		t.Fatalf("encode: %v", err)
	}
	var resp protocol.Response
	if err := dec.Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ID != 42 {
		t.Errorf("id mismatch: got %d", resp.ID)
	}
	if resp.Error != nil {
		t.Fatalf("status error: %s", resp.Error.Message)
	}
	var sr StatusResult
	if err := json.Unmarshal(resp.Result, &sr); err != nil {
		t.Fatalf("unmarshal status: %v", err)
	}
	if !sr.Running || sr.Sock != sock {
		t.Errorf("bad status: %+v", sr)
	}
}

// TestServe_Shutdown verifies that a "shutdown" request stops the daemon:
// Serve returns nil and the socket stops accepting new connections.
func TestServe_Shutdown(t *testing.T) {
	d := setupDaemon(t)
	sock := filepath.Join(t.TempDir(), "daemon.sock")
	d.Sock = sock

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveErr := make(chan error, 1)
	go func() { serveErr <- d.Serve(ctx) }()

	// Wait for socket to accept connections (retry dial until ready).
	var conn net.Conn
	var err error
	deadline, cancelWait := context.WithTimeout(ctx, awaitSockTimeout)
	for {
		conn, err = net.Dial("unix", sock)
		if err == nil {
			break
		}
		if deadline.Err() != nil {
			cancelWait()
			t.Fatalf("socket never accepted: %v", deadline.Err())
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancelWait()
	enc := json.NewEncoder(conn)
	dec := json.NewDecoder(conn)
	req := protocol.Request{Method: "shutdown", ID: 1}
	if err := enc.Encode(&req); err != nil {
		t.Fatalf("encode: %v", err)
	}
	var resp protocol.Response
	if err := dec.Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("shutdown error: %s", resp.Error.Message)
	}
	conn.Close()

	// Serve should return nil promptly.
	select {
	case err := <-serveErr:
		if err != nil {
			t.Errorf("Serve returned %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after shutdown")
	}

	// Socket file should be gone (Serve's defer ln.Close() doesn't unlink,
	// but a new dial must fail — listener closed).
	_, err = net.Dial("unix", sock)
	if err == nil {
		t.Error("dial succeeded after shutdown; listener should be closed")
	}
}
