package daemon

import (
	"context"
	"database/sql"
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
		// Default no-op stub so /bin/echo tests don't shell out to real
		// opencode. Individual tests that exercise capture override this.
		CaptureAgentSessionID: func(SpawnConfig) (string, error) { return "", nil },
		shutdownCh:            make(chan struct{}),
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
	d.SafetyNetEnabled = true
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

// ============================================
// Session resume schema migrations
// ============================================
func TestStore_SessionsSchemaMigrations(t *testing.T) {
	store := newTestStore(t)
	rows, err := store.db.Query(`PRAGMA table_info(sessions)`)
	if err != nil {
		t.Fatalf("table_info: %v", err)
	}
	defer rows.Close()
	got := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[name] = true
	}
	for _, col := range []string{"opencode_session_id", "name", "root_thread_id", "prefix"} {
		if !got[col] {
			t.Errorf("sessions.%s missing after migration", col)
		}
	}
}

// ============================================
// Session resume fields — Create stores root_thread_id, prefix, name
// ============================================

func TestSessions_CreateStoresResumeFields(t *testing.T) {
	store := newTestStore(t)
	store.db.Exec(`INSERT INTO workspaces(id, name) VALUES(1, 'ws')`)
	store.db.Exec(`INSERT INTO projects(id, workspace_id, name, path) VALUES(1, 1, 'p', '/tmp/p')`)
	s := &SessionStore{Store: store}
	sess, err := s.Create(
		/* projectID */ 1,
		/* binary */ "opencode",
		/* model */ "default",
		/* pid */ 0,
		/* taskMsgID */ 42,
		/* rootThreadID */ 42,
		/* prefix */ "abc12",
		/* name */ "abc12-dotfiles",
	)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	var gotOC sql.NullString
	var gotName, gotPrefix string
	var gotRoot int64
	err = store.db.QueryRow(
		`SELECT opencode_session_id, name, prefix, root_thread_id FROM sessions WHERE id=?`,
		sess.ID).Scan(&gotOC, &gotName, &gotPrefix, &gotRoot)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if gotOC.Valid || gotName != "abc12-dotfiles" || gotPrefix != "abc12" || gotRoot != 42 {
		t.Errorf("row: oc=%v name=%q prefix=%q root=%d", gotOC, gotName, gotPrefix, gotRoot)
	}
}

func TestSessions_SetAgentSessionID(t *testing.T) {
	store := newTestStore(t)
	store.db.Exec(`INSERT INTO workspaces(id, name) VALUES(1, 'ws')`)
	store.db.Exec(`INSERT INTO projects(id, workspace_id, name, path) VALUES(1, 1, 'p', '/tmp/p')`)
	s := &SessionStore{Store: store}
	sess, _ := s.Create(1, "opencode", "default", 0, 42, 42, "abc12", "abc12-dotfiles")
	if err := s.SetAgentSessionID(sess.ID, "ses_xyz789"); err != nil {
		t.Fatalf("SetAgentSessionID: %v", err)
	}
	var got string
	store.db.QueryRow(`SELECT opencode_session_id FROM sessions WHERE id=?`, sess.ID).Scan(&got)
	if got != "ses_xyz789" {
		t.Errorf("got %q want ses_xyz789", got)
	}
}

func TestWakeAgent_StoresRootThreadID(t *testing.T) {
	d := setupDaemon(t)
	d.Registry.AddWorkspace("companyA")
	userDir := t.TempDir()
	os.WriteFile(filepath.Join(userDir, "mouse.yaml"),
		[]byte("agent:\n  primary:\n    provider: opencode\na2a:\n  allow_inbound: true\n"), 0644)
	d.Registry.AddProject("companyA", "user-service", userDir)

	// Thread root wake: root_thread_id = msg.ID.
	params, _ := json.Marshal(map[string]any{
		"from": "companyA/payment", "to": "companyA/user-service", "content": "task 1",
	})
	resp := d.Handle(context.Background(), protocol.Request{Method: "post_message", Params: params, ID: 1})
	var pr PostResult
	json.Unmarshal(resp.Result, &pr)

	var rootTID int64
	d.Store.db.QueryRow(`SELECT root_thread_id FROM sessions WHERE task_msg_id=?`, pr.MessageID).Scan(&rootTID)
	if rootTID != pr.MessageID {
		t.Errorf("thread root: root_thread_id=%d want %d", rootTID, pr.MessageID)
	}
}

func TestHandle_CrossProjectDelegationInheritsRoot(t *testing.T) {
	d := setupDaemon(t)
	d.Registry.AddWorkspace("companyA")
	// Project A (caller) — already woken, has task_msg_id=500.
	aDir := t.TempDir()
	os.WriteFile(filepath.Join(aDir, "mouse.yaml"),
		[]byte("agent:\n  primary:\n    provider: opencode\na2a:\n  allow_inbound: true\n"), 0644)
	d.Registry.AddProject("companyA", "service-a", aDir)
	// Project B (callee) — inbound allowed.
	bDir := t.TempDir()
	os.WriteFile(filepath.Join(bDir, "mouse.yaml"),
		[]byte("agent:\n  primary:\n    provider: opencode\na2a:\n  allow_inbound: true\n"), 0644)
	d.Registry.AddProject("companyA", "service-b", bDir)
	// Seed: root thread 500 already exists, with service-a's session bound.
	d.Store.db.Exec(`INSERT INTO messages(id, channel_id, thread_id, from_project, to_project, content, status, ts)
		VALUES(500, 1, NULL, 'companyA/orchestrator', 'companyA/service-a', 'do X', 'message', datetime('now'))`)
	d.Store.db.Exec(`INSERT INTO sessions(project_id, agent_binary, model, status, pid, created_at, task_msg_id, root_thread_id)
		VALUES((SELECT id FROM projects WHERE name='service-a'), 'opencode', 'default', 'exited', 0, datetime('now'), 500, 500)`)

	// service-a (in its spawned session) calls service-b as a new thread root.
	// RootID=500 passed in params → service-b's session binds to root 500.
	params, _ := json.Marshal(map[string]any{
		"from": "companyA/service-a", "to": "companyA/service-b",
		"content": "sub-task", "root_id": 500,
	})
	resp := d.Handle(context.Background(), protocol.Request{Method: "post_message", Params: params, ID: 1})
	if resp.Error != nil {
		t.Fatalf("post: %s", resp.Error.Message)
	}
	var pr PostResult
	json.Unmarshal(resp.Result, &pr)

	var rootTID int64
	d.Store.db.QueryRow(`SELECT root_thread_id FROM sessions WHERE task_msg_id=?`, pr.MessageID).Scan(&rootTID)
	if rootTID != 500 {
		t.Fatalf("cross-project: root_thread_id=%d want 500", rootTID)
	}
}

func TestHandle_ReplyWithToWakesAgent(t *testing.T) {
	d := setupDaemon(t)
	d.Registry.AddWorkspace("companyA")
	userDir := t.TempDir()
	os.WriteFile(filepath.Join(userDir, "mouse.yaml"),
		[]byte("agent:\n  primary:\n    provider: opencode\na2a:\n  allow_inbound: true\n"), 0644)
	d.Registry.AddProject("companyA", "user-service", userDir)

	// Seed: thread root 500, agent already exited (no done reply yet).
	d.Store.db.Exec(`INSERT INTO messages(id, channel_id, thread_id, from_project, to_project, content, status, ts)
		VALUES(500, 1, NULL, 'companyA/payment', 'companyA/user-service', 'task', 'message', datetime('now'))`)
	d.Store.db.Exec(`INSERT INTO sessions(project_id, agent_binary, model, status, pid, created_at, task_msg_id, root_thread_id)
		VALUES((SELECT id FROM projects WHERE name='user-service'), 'opencode', 'default', 'exited', 0, datetime('now'), 500, 500)`)

	// Follow-up reply with to= — should wake the agent.
	params, _ := json.Marshal(map[string]any{
		"channel": 1, "thread_id": 500, "from": "companyA/payment",
		"to": "companyA/user-service", "content": "now do follow-up",
	})
	resp := d.Handle(context.Background(), protocol.Request{Method: "post_message", Params: params, ID: 1})
	if resp.Error != nil {
		t.Fatalf("post: %s", resp.Error.Message)
	}
	// A new session was created for the thread (root_thread_id=500).
	// task_msg_id on the new session is the reply's msg.ID, not 500 — so
	// query root_thread_id, which both sessions share.
	var sessCount int
	d.Store.db.QueryRow(`SELECT count(*) FROM sessions WHERE root_thread_id=500`).Scan(&sessCount)
	if sessCount != 2 { // 1 pre-existing + 1 new wake
		t.Fatalf("expected wake on reply-with-to, got %d sessions for thread 500", sessCount)
	}
}

// ============================================
// Wake guard — no wake on active session (done threads ARE re-wakeable)
// ============================================

// Re-waking a done thread is the follow-up resume path: the orchestrator
// posts a follow-up reply on a thread whose prior task already got a done
// reply, hmf resumes the agent's prior opencode session, agent keeps context.
func TestHandle_RewakeOnDoneThread(t *testing.T) {
	d := setupDaemon(t)
	d.Registry.AddWorkspace("companyA")
	userDir := t.TempDir()
	os.WriteFile(filepath.Join(userDir, "mouse.yaml"),
		[]byte("agent:\n  primary:\n    provider: opencode\na2a:\n  allow_inbound: true\n"), 0644)
	d.Registry.AddProject("companyA", "user-service", userDir)

	// Seed: thread root 500, an exited session bound to it, and a done reply.
	d.Store.db.Exec(`INSERT INTO messages(id, channel_id, thread_id, from_project, to_project, content, status, ts)
		VALUES(500, 1, NULL, 'companyA/payment', 'companyA/user-service', 'task', 'message', datetime('now'))`)
	d.Store.db.Exec(`INSERT INTO sessions(project_id, agent_binary, model, status, pid, created_at, task_msg_id, root_thread_id)
		VALUES((SELECT id FROM projects WHERE name='user-service'), 'opencode', 'default', 'exited', 0, datetime('now'), 500, 500)`)
	d.Store.db.Exec(`INSERT INTO messages(channel_id, thread_id, from_project, to_project, content, status, ts)
		VALUES(1, 500, 'companyA/user-service', 'companyA/payment', 'done', 'done', datetime('now'))`)

	// Follow-up reply with to= — SHOULD wake (resume path). Session is
	// exited, not active; done reply doesn't block re-wake anymore.
	params, _ := json.Marshal(map[string]any{
		"channel": 1, "thread_id": 500, "from": "companyA/payment",
		"to": "companyA/user-service", "content": "follow-up",
	})
	resp := d.Handle(context.Background(), protocol.Request{Method: "post_message", Params: params, ID: 1})
	if resp.Error != nil {
		t.Fatalf("post: %s", resp.Error.Message)
	}
	// A new session was created for this thread (count goes 1 → 2).
	var sessCount int
	d.Store.db.QueryRow(`SELECT count(*) FROM sessions WHERE root_thread_id=500`).Scan(&sessCount)
	if sessCount != 2 {
		t.Fatalf("expected re-wake on done thread, got %d sessions with root_thread_id=500", sessCount)
	}
}

func TestHandle_NoWakeOnActiveSession(t *testing.T) {
	d := setupDaemon(t)
	d.Registry.AddWorkspace("companyA")
	userDir := t.TempDir()
	os.WriteFile(filepath.Join(userDir, "mouse.yaml"),
		[]byte("agent:\n  primary:\n    provider: opencode\na2a:\n  allow_inbound: true\n"), 0644)
	d.Registry.AddProject("companyA", "user-service", userDir)

	// Seed: thread root 500 with an ACTIVE session bound. No done reply —
	// the done-reply guard won't fire; only the active-session guard should
	// suppress the wake here.
	d.Store.db.Exec(`INSERT INTO messages(id, channel_id, thread_id, from_project, to_project, content, status, ts)
		VALUES(500, 1, NULL, 'companyA/payment', 'companyA/user-service', 'task', 'message', datetime('now'))`)
	d.Store.db.Exec(`INSERT INTO sessions(project_id, agent_binary, model, status, pid, created_at, task_msg_id, root_thread_id)
		VALUES((SELECT id FROM projects WHERE name='user-service'), 'opencode', 'default', 'active', 0, datetime('now'), 500, 500)`)

	params, _ := json.Marshal(map[string]any{
		"channel": 1, "thread_id": 500, "from": "companyA/payment",
		"to": "companyA/user-service", "content": "follow-up",
	})
	resp := d.Handle(context.Background(), protocol.Request{Method: "post_message", Params: params, ID: 1})
	if resp.Error != nil {
		t.Fatalf("post: %s", resp.Error.Message)
	}
	// Pre-existing active session is the only one bound to root 500. A new
	// wake would create a second session with root_thread_id=500.
	var sessCount int
	d.Store.db.QueryRow(`SELECT count(*) FROM sessions WHERE root_thread_id=500`).Scan(&sessCount)
	if sessCount != 1 {
		t.Fatalf("expected no new session (1 pre-existing active), got %d with root_thread_id=500", sessCount)
	}
}

func TestWakeAgent_ResumeUsesCanonicalSessionID(t *testing.T) {
	d := setupDaemon(t)
	// Inject a fake OC-ID capturer so we don't depend on real opencode.
	captured := []string{"ses_fresh1", "ses_fresh2"}
	calls := 0
	d.CaptureAgentSessionID = func(cfg SpawnConfig) (string, error) {
		id := captured[calls%len(captured)]
		calls++
		return id, nil
	}
	// Stub the launcher's Spawn to record args without running opencode.
	var spawnArgs []string
	d.Launcher = &Launcher{Binary: "/bin/echo", SpawnFn: func(cfg SpawnConfig) (int, error) {
		spawnArgs = append(spawnArgs, cfg.AgentSessionID)
		return 1, nil
	}}

	d.Registry.AddWorkspace("companyA")
	userDir := t.TempDir()
	os.WriteFile(filepath.Join(userDir, "mouse.yaml"),
		[]byte("agent:\n  primary:\n    provider: opencode\na2a:\n  allow_inbound: true\n"), 0644)
	d.Registry.AddProject("companyA", "user-service", userDir)

	// First wake: thread root, fresh spawn, captures OC ID ses_fresh1.
	p1, _ := json.Marshal(map[string]any{"from": "companyA/payment", "to": "companyA/user-service", "content": "task 1"})
	r1 := d.Handle(context.Background(), protocol.Request{Method: "post_message", Params: p1, ID: 1})
	var pr1 PostResult
	json.Unmarshal(r1.Result, &pr1)

	// Mark the first session exited so the wake guard allows a follow-up.
	d.Store.db.Exec(`UPDATE sessions SET status='exited' WHERE task_msg_id=?`, pr1.MessageID)

	// Second wake: reply on the same thread, same project — should resume
	// ses_fresh1, NOT call the capturer again.
	p2, _ := json.Marshal(map[string]any{
		"channel": 1, "thread_id": pr1.MessageID, "from": "companyA/payment",
		"to": "companyA/user-service", "content": "follow-up",
	})
	r2 := d.Handle(context.Background(), protocol.Request{Method: "post_message", Params: p2, ID: 2})
	if r2.Error != nil {
		t.Fatalf("second post: %s", r2.Error.Message)
	}

	if len(spawnArgs) != 2 {
		t.Fatalf("expected 2 spawns, got %d", len(spawnArgs))
	}
	if spawnArgs[0] != "" {
		t.Errorf("first spawn: AgentSessionID=%q want empty (fresh)", spawnArgs[0])
	}
	if spawnArgs[1] != "ses_fresh1" {
		t.Errorf("second spawn: AgentSessionID=%q want ses_fresh1 (resume)", spawnArgs[1])
	}
	if calls != 1 {
		t.Errorf("capturer called %d times, want 1 (only on fresh spawn)", calls)
	}
}

func TestWakeAgent_PrefixGeneratedOnceAndInherited(t *testing.T) {
	d := setupDaemon(t)
	d.Registry.AddWorkspace("companyA")
	userDir := t.TempDir()
	os.WriteFile(filepath.Join(userDir, "mouse.yaml"),
		[]byte("agent:\n  primary:\n    provider: opencode\na2a:\n  allow_inbound: true\n"), 0644)
	d.Registry.AddProject("companyA", "user-service", userDir)

	// First wake: generates a prefix.
	p1, _ := json.Marshal(map[string]any{"from": "companyA/payment", "to": "companyA/user-service", "content": "t1"})
	r1 := d.Handle(context.Background(), protocol.Request{Method: "post_message", Params: p1, ID: 1})
	var pr1 PostResult
	json.Unmarshal(r1.Result, &pr1)

	var prefix1, name1 string
	d.Store.db.QueryRow(`SELECT prefix, name FROM sessions WHERE task_msg_id=?`, pr1.MessageID).Scan(&prefix1, &name1)
	if len(prefix1) != 5 {
		t.Fatalf("prefix len: got %d want 5", len(prefix1))
	}
	if name1 != prefix1+"-user-service" {
		t.Errorf("name: got %q want %q-user-service", name1, prefix1)
	}

	// Mark first session exited so wake guard allows the second.
	d.Store.db.Exec(`UPDATE sessions SET status='exited' WHERE task_msg_id=?`, pr1.MessageID)

	// Second wake (reply, same thread root): should inherit prefix1.
	p2, _ := json.Marshal(map[string]any{
		"channel": 1, "thread_id": pr1.MessageID, "from": "companyA/payment",
		"to": "companyA/user-service", "content": "t2",
	})
	r2 := d.Handle(context.Background(), protocol.Request{Method: "post_message", Params: p2, ID: 2})
	if r2.Error != nil {
		t.Fatalf("second post: %s", r2.Error.Message)
	}

	// Latest session for this root is the second wake's. Its prefix must
	// equal prefix1 (inherited), not a freshly-generated one.
	var latestPrefix string
	d.Store.db.QueryRow(
		`SELECT prefix FROM sessions WHERE root_thread_id=? ORDER BY id DESC LIMIT 1`,
		pr1.MessageID).Scan(&latestPrefix)
	if latestPrefix != prefix1 {
		t.Errorf("second wake prefix: got %q want %q (inherited)", latestPrefix, prefix1)
	}
}
