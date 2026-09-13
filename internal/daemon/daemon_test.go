package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
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
		Todos:    &TodoStore{Store: store},
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
	userDir := t.TempDir()
	os.WriteFile(filepath.Join(userDir, "mouse.yaml"), []byte("agent:\n  primary:\n    provider: opencode\na2a:\n  allow_inbound: true\n"), 0644)
	d.Registry.AddProject("user-service", userDir)

	// Post a task to general mentioning @user-service — thread root.
	params, _ := json.Marshal(map[string]any{
		"from":    "payment-service",
		"to":      "user-service",
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

// TestHandle_SyntheticBlockedReplyOnSilentExit: /bin/echo exits without a
// done reply → daemon posts synthetic BLOCKED reply so orchestrator stops polling.
func TestHandle_SyntheticBlockedReplyOnSilentExit(t *testing.T) {
	d := setupDaemon(t)
	d.SafetyNetEnabled = true
	userDir := t.TempDir()
	os.WriteFile(filepath.Join(userDir, "mouse.yaml"), []byte("agent:\n  primary:\n    provider: opencode\na2a:\n  allow_inbound: true\n"), 0644)
	d.Registry.AddProject("user-service", userDir)

	params, _ := json.Marshal(map[string]any{
		"from":    "payment-service",
		"to":      "user-service",
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
	userDir := t.TempDir()
	os.WriteFile(filepath.Join(userDir, "mouse.yaml"), []byte("agent:\n  primary:\n    provider: opencode\n"), 0644)
	d.Registry.AddProject("user-service", userDir)

	params, _ := json.Marshal(map[string]any{"from": "payment", "to": "user-service", "content": "x"})
	resp := d.Handle(context.Background(), protocol.Request{Method: "post_message", Params: params, ID: 1})
	if resp.Error == nil {
		t.Fatal("expected inbound-denied error")
	}
}

func TestHandle_PostAndRead(t *testing.T) {
	d := setupDaemon(t)
	d.Store.db.Exec(`INSERT INTO channels(id, name, type) VALUES(10, 'dm', 'dm')`)

	postParams, _ := json.Marshal(map[string]any{
		"channel": 10, "from": "payment", "to": "user", "content": "hello",
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
	if len(msgs) != 1 || msgs[0].Content != "hello" || msgs[0].FromProject != "payment" {
		t.Errorf("got %+v", msgs)
	}
}

func TestHandle_PostToUnregisteredAgentSkipsWake(t *testing.T) {
	d := setupDaemon(t)

	// Addressing an unregistered agent: message posts, no wake (mailbox semantics).
	params, _ := json.Marshal(map[string]any{"from": "payment", "to": "ghost", "content": "hello"})
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

// postTask sets up a thread-root message + optional session linked by
// task_msg_id, returning the thread root id.
func postTask(t *testing.T, d *Daemon, sessionStatus string, exitCode int, withDone bool) int64 {
	t.Helper()
	d.Store.db.Exec(`INSERT INTO projects(id, name, path) VALUES(1, 'user-service', '/tmp/user')`)

	// Thread root message in the general channel.
	res, err := d.Store.db.Exec(
		`INSERT INTO messages(channel_id, thread_id, from_project, to_project, content, status, ts)
		 VALUES(1, NULL, 'payment', 'user-service', 'task', 'message', datetime('now'))`)
	if err != nil {
		t.Fatal(err)
	}
	parentID, _ := res.LastInsertId()

	if sessionStatus != "" {
		d.Store.db.Exec(
			`INSERT INTO sessions(project_id, agent_binary, model, status, pid, created_at, task_msg_id, root_thread_id, exit_code)
			 VALUES(1, 'opencode', 'default', ?, 12345, datetime('now'), ?, ?, ?)`,
			sessionStatus, parentID, parentID, exitCode)
	}
	if withDone {
		d.Store.db.Exec(
			`INSERT INTO messages(channel_id, thread_id, from_project, to_project, content, status, ts)
			 VALUES(1, ?, 'user-service', 'payment', 'done', 'done', datetime('now'))`,
			parentID)
	}
	return parentID
}

func TestHandle_TaskStatus_Working(t *testing.T) {
	d := setupDaemon(t)
	parentID := postTask(t, d, "active", 0, false)
	resp := d.Handle(context.Background(), protocol.Request{
		Method: "task_status",
		Params: mustJSON(t, map[string]any{"message_id": parentID}),
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
	parentID := postTask(t, d, "exited", 0, true)
	resp := d.Handle(context.Background(), protocol.Request{
		Method: "task_status",
		Params: mustJSON(t, map[string]any{"message_id": parentID}),
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
	parentID := postTask(t, d, "failed", 1, false)
	resp := d.Handle(context.Background(), protocol.Request{
		Method: "task_status",
		Params: mustJSON(t, map[string]any{"message_id": parentID}),
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
	parentID := postTask(t, d, "", 0, false)
	resp := d.Handle(context.Background(), protocol.Request{
		Method: "task_status",
		Params: mustJSON(t, map[string]any{"message_id": parentID}),
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

// task_status must answer instantly: the push-wake contract means callers
// never wait server-side. An unresolved task returns has_done=false fast.
func TestTaskStatusReturnsImmediately(t *testing.T) {
	d := setupDaemon(t)
	d.Store.db.Exec(`INSERT INTO projects(id, name, path) VALUES(1, 'child', '/tmp/child')`)
	d.Store.db.Exec(`INSERT INTO messages(id, channel_id, thread_id, from_project, to_project, content, status, ts)
		VALUES(900, 1, NULL, 'parent', 'child', 'do X', 'message', datetime('now'))`)
	// Active session keeps the status non-terminal — the case that used to block.
	d.Store.db.Exec(`INSERT INTO sessions(project_id, agent_binary, model, status, pid, created_at, task_msg_id, root_thread_id)
		VALUES(1, 'opencode', 'default', 'active', 0, datetime('now'), 900, 900)`)

	start := time.Now()
	params, _ := json.Marshal(map[string]any{"message_id": 900})
	resp := d.Handle(context.Background(), protocol.Request{Method: "task_status", Params: params, ID: 1})
	if resp.Error != nil {
		t.Fatalf("task_status: %s", resp.Error.Message)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("task_status blocked %v — must be instant", elapsed)
	}
	var res TaskStatusResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatal(err)
	}
	if res.HasDone {
		t.Fatal("fresh task must not report has_done")
	}
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, _ := json.Marshal(v)
	return b
}

// TestServe_Smoke: start Serve, send one status request over the socket.
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

// TestServe_Shutdown: a "shutdown" request stops the daemon + socket.
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
	store.db.Exec(`INSERT INTO projects(id, name, path) VALUES(1, 'p', '/tmp/p')`)
	s := &SessionStore{Store: store}
	sess, err := s.Create(
		/* projectID */ 1,
		/* binary */ "opencode",
		/* model */ "default",
		/* pid */ 0,
		/* taskMsgID */ 42,
		/* parentID */ 42,
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
	store.db.Exec(`INSERT INTO projects(id, name, path) VALUES(1, 'p', '/tmp/p')`)
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

func TestWakeAgent_StoresParentID(t *testing.T) {
	d := setupDaemon(t)
	userDir := t.TempDir()
	os.WriteFile(filepath.Join(userDir, "mouse.yaml"),
		[]byte("agent:\n  primary:\n    provider: opencode\na2a:\n  allow_inbound: true\n"), 0644)
	d.Registry.AddProject("user-service", userDir)

	// Thread root wake: root_thread_id = msg.ID.
	params, _ := json.Marshal(map[string]any{
		"from": "payment", "to": "user-service", "content": "task 1",
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
	// Project A (caller) — already woken, has task_msg_id=500.
	aDir := t.TempDir()
	// service-a delegates outward, so it must declare outbound consent.
	os.WriteFile(filepath.Join(aDir, "mouse.yaml"),
		[]byte("agent:\n  primary:\n    provider: opencode\na2a:\n  allow_inbound: true\n  allow_outbound: true\n"), 0644)
	d.Registry.AddProject("service-a", aDir)
	// Project B (callee) — inbound allowed.
	bDir := t.TempDir()
	os.WriteFile(filepath.Join(bDir, "mouse.yaml"),
		[]byte("agent:\n  primary:\n    provider: opencode\na2a:\n  allow_inbound: true\n"), 0644)
	d.Registry.AddProject("service-b", bDir)
	// Seed: root thread 500 already exists, with service-a's session bound.
	d.Store.db.Exec(`INSERT INTO messages(id, channel_id, thread_id, from_project, to_project, content, status, ts)
		VALUES(500, 1, NULL, 'orchestrator', 'service-a', 'do X', 'message', datetime('now'))`)
	d.Store.db.Exec(`INSERT INTO sessions(project_id, agent_binary, model, status, pid, created_at, task_msg_id, root_thread_id)
		VALUES((SELECT id FROM projects WHERE name='service-a'), 'opencode', 'default', 'exited', 0, datetime('now'), 500, 500)`)

	// service-a (in its spawned session) calls service-b as a new thread root.
	// ParentID=500 passed in params → service-b's session binds to root 500.
	params, _ := json.Marshal(map[string]any{
		"from": "service-a", "to": "service-b",
		"content": "sub-task", "parent_id": 500,
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

// Delegation via `thread_id` (not explicit `parent_id`): the spawned agent
// must be told to reply on the resolved root, or task_status never sees has_done.
func TestWakeAgent_TaskMsgIDMatchesRootForTaskStatus(t *testing.T) {
	var captured SpawnConfig
	d := setupDaemon(t)
	d.Launcher = &Launcher{Binary: "/bin/echo", SpawnFn: func(cfg SpawnConfig) (int, error) {
		captured = cfg
		return 1, nil
	}}
	aDir := t.TempDir()
	// service-a delegates outward, so it must declare outbound consent.
	os.WriteFile(filepath.Join(aDir, "mouse.yaml"),
		[]byte("agent:\n  primary:\n    provider: opencode\na2a:\n  allow_inbound: true\n  allow_outbound: true\n"), 0644)
	d.Registry.AddProject("service-a", aDir)
	bDir := t.TempDir()
	os.WriteFile(filepath.Join(bDir, "mouse.yaml"),
		[]byte("agent:\n  primary:\n    provider: opencode\na2a:\n  allow_inbound: true\n"), 0644)
	d.Registry.AddProject("service-b", bDir)
	// Seed: root thread 500, service-a's session bound.
	d.Store.db.Exec(`INSERT INTO messages(id, channel_id, thread_id, from_project, to_project, content, status, ts)
		VALUES(500, 1, NULL, 'orchestrator', 'service-a', 'do X', 'message', datetime('now'))`)
	d.Store.db.Exec(`INSERT INTO sessions(project_id, agent_binary, model, status, pid, created_at, task_msg_id, root_thread_id)
		VALUES((SELECT id FROM projects WHERE name='service-a'), 'opencode', 'default', 'exited', 0, datetime('now'), 500, 500)`)

	// service-a delegates via thread_id=500 (the real-world path — no
	// explicit parent_id). This message itself becomes a reply on thread
	// 500 while also spawning its own session.
	params, _ := json.Marshal(map[string]any{
		"from": "service-a", "to": "service-b",
		"content": "sub-task", "thread_id": 500,
	})
	resp := d.Handle(context.Background(), protocol.Request{Method: "post_message", Params: params, ID: 1})
	if resp.Error != nil {
		t.Fatalf("post: %s", resp.Error.Message)
	}
	var pr PostResult
	json.Unmarshal(resp.Result, &pr)

	if captured.TaskMsgID != 500 {
		t.Fatalf("spawned TaskMsgID=%d, want root 500 (got the delegating message's own id %d instead)", captured.TaskMsgID, pr.MessageID)
	}

	// service-b replies exactly like `hmf done`/[REPLY RULE] would: thread_id
	// = HMF_TASK_MSG_ID = captured.TaskMsgID.
	doneParams, _ := json.Marshal(map[string]any{
		"from": "service-b", "thread_id": captured.TaskMsgID,
		"content": "done", "status": "done",
	})
	if resp := d.Handle(context.Background(), protocol.Request{Method: "post_message", Params: doneParams, ID: 2}); resp.Error != nil {
		t.Fatalf("done reply: %s", resp.Error.Message)
	}

	// The caller tracks its delegation by the message id it got back
	// (pr.MessageID) — task_status on THAT id must see has_done=true.
	tsParams, _ := json.Marshal(map[string]any{"message_id": pr.MessageID})
	tsResp := d.Handle(context.Background(), protocol.Request{Method: "task_status", Params: tsParams, ID: 3})
	if tsResp.Error != nil {
		t.Fatalf("task_status: %s", tsResp.Error.Message)
	}
	var ts TaskStatusResult
	json.Unmarshal(tsResp.Result, &ts)
	if !ts.HasDone {
		t.Fatalf("task_status(message_id=%d).has_done = false, want true", pr.MessageID)
	}
}

// Repeat task_status calls return instantly — wake-on-done pushes completion,
// so there is no server-side pacing to lean on anymore.
func TestHandle_TaskStatusRepeatCallInstant(t *testing.T) {
	d := setupDaemon(t)
	userDir := t.TempDir()
	os.WriteFile(filepath.Join(userDir, "mouse.yaml"),
		[]byte("agent:\n  primary:\n    provider: opencode\na2a:\n  allow_inbound: true\n"), 0644)
	d.Registry.AddProject("user-service", userDir)

	d.Store.db.Exec(`INSERT INTO messages(id, channel_id, thread_id, from_project, to_project, content, status, ts)
		VALUES(500, 1, NULL, 'payment', 'user-service', 'task', 'message', datetime('now'))`)
	d.Store.db.Exec(`INSERT INTO sessions(project_id, agent_binary, model, status, pid, created_at, task_msg_id, root_thread_id)
		VALUES((SELECT id FROM projects WHERE name='user-service'), 'opencode', 'default', 'active', 0, datetime('now'), 500, 500)`)

	params, _ := json.Marshal(map[string]any{"message_id": 500})

	for i := 1; i <= 2; i++ {
		start := time.Now()
		resp := d.Handle(context.Background(), protocol.Request{Method: "task_status", Params: params, ID: int64(i)})
		if resp.Error != nil {
			t.Fatalf("call %d: %s", i, resp.Error.Message)
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("call %d took %v — task_status must be instant now", i, elapsed)
		}
		var ts TaskStatusResult
		json.Unmarshal(resp.Result, &ts)
		if ts.HasDone {
			t.Fatalf("call %d: fresh task must not report has_done", i)
		}
	}
}

// A done reply already landed must not get overwritten by a nonzero exit
// code — should read "exited", not "failed".
func TestWakeAgent_KillAfterDoneMarksExitedNotFailed(t *testing.T) {
	d := setupDaemon(t)
	userDir := t.TempDir()
	os.WriteFile(filepath.Join(userDir, "mouse.yaml"),
		[]byte("agent:\n  primary:\n    provider: opencode\na2a:\n  allow_inbound: true\n"), 0644)
	d.Registry.AddProject("user-service", userDir)

	// OnExit fires async in real Spawn, after SetStatus("active") — call it
	// after d.Handle returns below, not inline, or it races that ordering.
	var captured SpawnConfig
	d.Launcher = &Launcher{Binary: "/bin/echo", SpawnFn: func(cfg SpawnConfig) (int, error) {
		captured = cfg
		return 1, nil
	}}

	params, _ := json.Marshal(map[string]any{
		"from": "payment", "to": "user-service", "content": "task",
	})
	resp := d.Handle(context.Background(), protocol.Request{Method: "post_message", Params: params, ID: 1})
	if resp.Error != nil {
		t.Fatalf("post: %s", resp.Error.Message)
	}
	var pr PostResult
	json.Unmarshal(resp.Result, &pr)

	// Simulate: agent posts its done reply, then the process gets killed
	// afterward (e.g. a resumed session that won't self-exit) — exit code
	// is nonzero despite the task having succeeded.
	done, _ := json.Marshal(map[string]any{
		"from": "user-service", "thread_id": captured.TaskMsgID,
		"content": "done", "status": "done",
	})
	if resp := d.Handle(context.Background(), protocol.Request{Method: "post_message", Params: done, ID: 99}); resp.Error != nil {
		t.Fatalf("done reply: %s", resp.Error.Message)
	}
	captured.OnExit(-1) // SIGTERM-style exit code

	var status string
	var exitCode int
	d.Store.db.QueryRow(`SELECT status, exit_code FROM sessions WHERE task_msg_id=?`, pr.MessageID).Scan(&status, &exitCode)
	if status != "exited" {
		t.Fatalf("status=%q exit_code=%d, want status=exited (done reply already existed before kill)", status, exitCode)
	}
}

// hasStatus reports whether any message in msgs carries status.
func hasStatus(msgs []Message, status string) bool {
	for _, m := range msgs {
		if m.Status == status {
			return true
		}
	}
	return false
}

// Mirror of the above: an agent that dies nonzero having posted nothing must
// land as failed AND get the synthetic BLOCKED reply — without it the parent
// polls a dead session forever.
func TestWakeAgent_ExitWithoutDoneMarksFailedAndPostsBlocked(t *testing.T) {
	d := setupDaemon(t)
	d.SafetyNetEnabled = true
	userDir := t.TempDir()
	os.WriteFile(filepath.Join(userDir, "mouse.yaml"),
		[]byte("agent:\n  primary:\n    provider: opencode\na2a:\n  allow_inbound: true\n"), 0644)
	d.Registry.AddProject("user-service", userDir)

	var captured SpawnConfig
	d.Launcher = &Launcher{SpawnFn: func(cfg SpawnConfig) (int, error) {
		captured = cfg
		return 1, nil
	}}

	params, _ := json.Marshal(map[string]any{
		"from": "payment", "to": "user-service", "content": "task",
	})
	resp := d.Handle(context.Background(), protocol.Request{Method: "post_message", Params: params, ID: 1})
	if resp.Error != nil {
		t.Fatalf("post: %s", resp.Error.Message)
	}
	var pr PostResult
	json.Unmarshal(resp.Result, &pr)

	captured.OnExit(1) // crashed, no done reply

	var status string
	var exitCode int
	d.Store.db.QueryRow(`SELECT status, exit_code FROM sessions WHERE task_msg_id=?`, pr.MessageID).Scan(&status, &exitCode)
	if status != "failed" || exitCode != 1 {
		t.Errorf("status=%q exit_code=%d, want failed/1", status, exitCode)
	}
	var content string
	if err := d.Store.db.QueryRow(
		`SELECT content FROM messages WHERE thread_id=? AND status='done'`, pr.MessageID).Scan(&content); err != nil {
		t.Fatalf("no safety-net reply on thread %d: %v", pr.MessageID, err)
	}
	if !strings.HasPrefix(content, "BLOCKED:") {
		t.Errorf("safety-net reply = %q, want a BLOCKED notice", content)
	}
}

// Dead PID with a done reply already posted → exited; dead PID with no
// done reply → failed + synthetic BLOCKED reply.
func TestReconcileOrphanedSessions(t *testing.T) {
	d := setupDaemon(t)
	d.SafetyNetEnabled = true
	userDir := t.TempDir()
	os.WriteFile(filepath.Join(userDir, "mouse.yaml"),
		[]byte("agent:\n  primary:\n    provider: opencode\na2a:\n  allow_inbound: true\n"), 0644)
	d.Registry.AddProject("user-service", userDir)

	// PID 999999999 is never a real live process. Root msg 700, completed.
	d.Store.db.Exec(`INSERT INTO messages(id, channel_id, thread_id, from_project, to_project, content, status, ts)
		VALUES(700, 1, NULL, 'payment', 'user-service', 'task', 'message', datetime('now'))`)
	d.Store.db.Exec(`INSERT INTO messages(channel_id, thread_id, from_project, to_project, content, status, ts)
		VALUES(1, 700, 'user-service', 'payment', 'done', 'done', datetime('now'))`)
	d.Store.db.Exec(`INSERT INTO sessions(project_id, agent_binary, model, status, pid, created_at, task_msg_id, root_thread_id)
		VALUES((SELECT id FROM projects WHERE name='user-service'), 'opencode', 'default', 'active', 999999999, datetime('now'), 700, 700)`)

	// Root msg 701, orphaned mid-task, no done reply ever posted.
	d.Store.db.Exec(`INSERT INTO messages(id, channel_id, thread_id, from_project, to_project, content, status, ts)
		VALUES(701, 1, NULL, 'payment', 'user-service', 'task 2', 'message', datetime('now'))`)
	d.Store.db.Exec(`INSERT INTO sessions(project_id, agent_binary, model, status, pid, created_at, task_msg_id, root_thread_id)
		VALUES((SELECT id FROM projects WHERE name='user-service'), 'opencode', 'default', 'active', 999999998, datetime('now'), 701, 701)`)

	// A genuinely live process (this test's own PID) must be left alone.
	d.Store.db.Exec(`INSERT INTO messages(id, channel_id, thread_id, from_project, to_project, content, status, ts)
		VALUES(702, 1, NULL, 'payment', 'user-service', 'task 3', 'message', datetime('now'))`)
	d.Store.db.Exec(`INSERT INTO sessions(project_id, agent_binary, model, status, pid, created_at, task_msg_id, root_thread_id)
		VALUES((SELECT id FROM projects WHERE name='user-service'), 'opencode', 'default', 'active', ?, datetime('now'), 702, 702)`, os.Getpid())

	d.reconcileOrphanedSessions()

	var status700, status701, status702 string
	d.Store.db.QueryRow(`SELECT status FROM sessions WHERE root_thread_id=700`).Scan(&status700)
	d.Store.db.QueryRow(`SELECT status FROM sessions WHERE root_thread_id=701`).Scan(&status701)
	d.Store.db.QueryRow(`SELECT status FROM sessions WHERE root_thread_id=702`).Scan(&status702)

	if status700 != "exited" {
		t.Errorf("dead PID + done reply already posted: status=%q, want exited", status700)
	}
	if status701 != "failed" {
		t.Errorf("dead PID + no done reply: status=%q, want failed", status701)
	}
	if status702 != "active" {
		t.Errorf("live PID must be left alone: status=%q, want active", status702)
	}

	// Orphan with no done reply gets a synthetic BLOCKED reply, matching the
	// live OnExit safety-net behavior.
	var blockedCount int
	d.Store.db.QueryRow(`SELECT count(*) FROM messages WHERE thread_id=701 AND status='done' AND content LIKE 'BLOCKED:%'`).Scan(&blockedCount)
	if blockedCount != 1 {
		t.Errorf("expected 1 synthetic BLOCKED reply for orphaned thread 701, got %d", blockedCount)
	}
	// The completed one must NOT get a duplicate/extra done reply.
	var doneCount700 int
	d.Store.db.QueryRow(`SELECT count(*) FROM messages WHERE thread_id=700 AND status='done'`).Scan(&doneCount700)
	if doneCount700 != 1 {
		t.Errorf("thread 700 already had its done reply, reconcile must not add another — got %d done messages", doneCount700)
	}
}

func TestHandle_ReplyWithToWakesAgent(t *testing.T) {
	d := setupDaemon(t)
	userDir := t.TempDir()
	os.WriteFile(filepath.Join(userDir, "mouse.yaml"),
		[]byte("agent:\n  primary:\n    provider: opencode\na2a:\n  allow_inbound: true\n"), 0644)
	d.Registry.AddProject("user-service", userDir)

	// Seed: thread root 500, agent already exited (no done reply yet).
	d.Store.db.Exec(`INSERT INTO messages(id, channel_id, thread_id, from_project, to_project, content, status, ts)
		VALUES(500, 1, NULL, 'payment', 'user-service', 'task', 'message', datetime('now'))`)
	d.Store.db.Exec(`INSERT INTO sessions(project_id, agent_binary, model, status, pid, created_at, task_msg_id, root_thread_id)
		VALUES((SELECT id FROM projects WHERE name='user-service'), 'opencode', 'default', 'exited', 0, datetime('now'), 500, 500)`)

	// Follow-up reply with to= — should wake the agent.
	params, _ := json.Marshal(map[string]any{
		"channel": 1, "thread_id": 500, "from": "payment",
		"to": "user-service", "content": "now do follow-up",
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

// A reply that forgets `to` should still wake the thread's original recipient.
func TestHandle_ReplyWithoutToAutoFillsFromRoot(t *testing.T) {
	d := setupDaemon(t)
	userDir := t.TempDir()
	os.WriteFile(filepath.Join(userDir, "mouse.yaml"),
		[]byte("agent:\n  primary:\n    provider: opencode\na2a:\n  allow_inbound: true\n"), 0644)
	d.Registry.AddProject("user-service", userDir)

	// Seed: thread root 500 addressed to user-service, agent already exited.
	d.Store.db.Exec(`INSERT INTO messages(id, channel_id, thread_id, from_project, to_project, content, status, ts)
		VALUES(500, 1, NULL, 'payment', 'user-service', 'task', 'message', datetime('now'))`)
	d.Store.db.Exec(`INSERT INTO sessions(project_id, agent_binary, model, status, pid, created_at, task_msg_id, root_thread_id)
		VALUES((SELECT id FROM projects WHERE name='user-service'), 'opencode', 'default', 'exited', 0, datetime('now'), 500, 500)`)

	// Follow-up reply with NO `to` — should still wake user-service.
	params, _ := json.Marshal(map[string]any{
		"channel": 1, "thread_id": 500, "from": "payment",
		"status": "in_progress", "content": "retry, still nothing happened",
	})
	resp := d.Handle(context.Background(), protocol.Request{Method: "post_message", Params: params, ID: 1})
	if resp.Error != nil {
		t.Fatalf("post: %s", resp.Error.Message)
	}
	var sessCount int
	d.Store.db.QueryRow(`SELECT count(*) FROM sessions WHERE root_thread_id=500`).Scan(&sessCount)
	if sessCount != 2 {
		t.Fatalf("expected auto-filled `to` to wake user-service, got %d sessions for thread 500", sessCount)
	}
}

// The worker's own `done` reply must NOT auto-fill `to` — that would resolve
// to the thread root's recipient (the worker itself) and re-wake it. Waking
// the ORIGINATOR on done is the push-wake contract (see TestDoneReplyWakesOriginator);
// here only the worker's session count must stay flat.
func TestHandle_DoneReplyWithoutToDoesNotAutoWake(t *testing.T) {
	d := setupDaemon(t)
	userDir := t.TempDir()
	os.WriteFile(filepath.Join(userDir, "mouse.yaml"),
		[]byte("agent:\n  primary:\n    provider: opencode\na2a:\n  allow_inbound: true\n"), 0644)
	d.Registry.AddProject("user-service", userDir)
	// payment is registered too — its wake on done is fine; the worker's isn't.
	paymentDir := t.TempDir()
	os.WriteFile(filepath.Join(paymentDir, "mouse.yaml"),
		[]byte("agent:\n  primary:\n    provider: opencode\na2a:\n  allow_inbound: true\n"), 0644)
	d.Registry.AddProject("payment", paymentDir)

	d.Store.db.Exec(`INSERT INTO messages(id, channel_id, thread_id, from_project, to_project, content, status, ts)
		VALUES(500, 1, NULL, 'payment', 'user-service', 'task', 'message', datetime('now'))`)
	d.Store.db.Exec(`INSERT INTO sessions(project_id, agent_binary, model, status, pid, created_at, task_msg_id, root_thread_id)
		VALUES((SELECT id FROM projects WHERE name='user-service'), 'opencode', 'default', 'exited', 0, datetime('now'), 500, 500)`)

	// Worker's completion reply — no `to`, status=done.
	params, _ := json.Marshal(map[string]any{
		"channel": 1, "thread_id": 500, "from": "user-service",
		"status": "done", "content": "done, files changed: x.go",
	})
	resp := d.Handle(context.Background(), protocol.Request{Method: "post_message", Params: params, ID: 1})
	if resp.Error != nil {
		t.Fatalf("post: %s", resp.Error.Message)
	}
	var sessCount int
	d.Store.db.QueryRow(
		`SELECT count(*) FROM sessions WHERE root_thread_id=500
		 AND project_id=(SELECT id FROM projects WHERE name='user-service')`).Scan(&sessCount)
	if sessCount != 1 {
		t.Fatalf("done reply must not auto-fill to and re-wake the worker, got %d worker sessions for thread 500", sessCount)
	}
}

// The core push-wake contract: a child's done reply resumes the originator
// project's opencode session with the summary, instead of waiting for a
// parent poll. The parent's session lives on its own upper thread (499), so
// the wake must find it project-scoped.
func TestDoneReplyWakesOriginator(t *testing.T) {
	var spawned []SpawnConfig
	d := setupDaemon(t)
	d.Launcher = &Launcher{Binary: "/bin/echo", SpawnFn: func(cfg SpawnConfig) (int, error) {
		spawned = append(spawned, cfg)
		return 1, nil
	}}
	aDir := t.TempDir()
	os.WriteFile(filepath.Join(aDir, "mouse.yaml"),
		[]byte("agent:\n  primary:\n    provider: opencode\na2a:\n  allow_inbound: true\n  allow_outbound: true\n"), 0644)
	d.Registry.AddProject("parent", aDir)
	bDir := t.TempDir()
	os.WriteFile(filepath.Join(bDir, "mouse.yaml"),
		[]byte("agent:\n  primary:\n    provider: opencode\na2a:\n  allow_inbound: true\n"), 0644)
	d.Registry.AddProject("child", bDir)

	// Root task 500: parent → child. The parent's OWN session is on its
	// upper thread 499 — NOT on thread 500.
	d.Store.db.Exec(`INSERT INTO messages(id, channel_id, thread_id, from_project, to_project, content, status, ts)
		VALUES(500, 1, NULL, 'parent', 'child', 'do X', 'message', datetime('now'))`)
	d.Store.db.Exec(`INSERT INTO sessions(project_id, agent_binary, model, status, pid, created_at, task_msg_id, root_thread_id, opencode_session_id)
		VALUES((SELECT id FROM projects WHERE name='parent'), 'opencode', 'default', 'exited', 0, datetime('now'), 499, 499, 'ses_parent')`)

	done, _ := json.Marshal(map[string]any{
		"from": "child", "thread_id": 500,
		"content": "did X, tests pass", "status": "done",
	})
	resp := d.Handle(context.Background(), protocol.Request{Method: "post_message", Params: done, ID: 1})
	if resp.Error != nil {
		t.Fatalf("done reply: %s", resp.Error.Message)
	}
	if len(spawned) != 1 {
		t.Fatalf("want 1 parent wake spawn, got %d", len(spawned))
	}
	cfg := spawned[0]
	if cfg.AgentSessionID != "ses_parent" {
		t.Fatalf("wake resumed %q, want parent's ses_parent", cfg.AgentSessionID)
	}
	if !strings.Contains(cfg.Task, "did X, tests pass") {
		t.Fatalf("wake prompt missing child summary, got: %q", cfg.Task)
	}
	// The done reply itself is in the thread.
	msgs, err := d.Comms.ReadThread(500)
	if err != nil {
		t.Fatal(err)
	}
	last := msgs[len(msgs)-1]
	if last.Status != "done" || last.Content != "did X, tests pass" {
		t.Fatalf("thread tail = %+v, want the done reply", last)
	}
}

// Done-wakes are continuations, not new delegations: the prompt must say the
// sub-agent finished, and the daemon must not fake an ack on a closed thread.
func TestDoneWakeEntrypointAndNoAck(t *testing.T) {
	var spawned []SpawnConfig
	d := setupDaemon(t)
	d.Launcher = &Launcher{Binary: "/bin/echo", SpawnFn: func(cfg SpawnConfig) (int, error) {
		spawned = append(spawned, cfg)
		return 1, nil
	}}
	aDir := t.TempDir()
	os.WriteFile(filepath.Join(aDir, "mouse.yaml"),
		[]byte("agent:\n  primary:\n    provider: opencode\na2a:\n  allow_inbound: true\n  allow_outbound: true\n"), 0644)
	d.Registry.AddProject("parent", aDir)
	bDir := t.TempDir()
	os.WriteFile(filepath.Join(bDir, "mouse.yaml"),
		[]byte("agent:\n  primary:\n    provider: opencode\na2a:\n  allow_inbound: true\n"), 0644)
	d.Registry.AddProject("child", bDir)
	d.Store.db.Exec(`INSERT INTO messages(id, channel_id, thread_id, from_project, to_project, content, status, ts)
		VALUES(700, 1, NULL, 'parent', 'child', 'do X', 'message', datetime('now'))`)
	d.Store.db.Exec(`INSERT INTO sessions(project_id, agent_binary, model, status, pid, created_at, task_msg_id, root_thread_id, opencode_session_id)
		VALUES((SELECT id FROM projects WHERE name='parent'), 'opencode', 'default', 'exited', 0, datetime('now'), 700, 700, 'ses_parent')`)

	done, _ := json.Marshal(map[string]any{
		"from": "child", "thread_id": 700, "content": "done: all green", "status": "done",
	})
	if resp := d.Handle(context.Background(), protocol.Request{Method: "post_message", Params: done, ID: 1}); resp.Error != nil {
		t.Fatalf("done reply: %s", resp.Error.Message)
	}
	if len(spawned) != 1 {
		t.Fatalf("want 1 spawn, got %d", len(spawned))
	}
	if !strings.HasPrefix(spawned[0].Task, "[TASK FINISHED]") {
		t.Fatalf("entrypoint = %q, want [TASK FINISHED] prefix", spawned[0].Task[:40])
	}
	// Thread must contain ONLY the root + the done reply — no synthetic ack.
	msgs, _ := d.Comms.ReadThread(700)
	for _, m := range msgs {
		if m.Status == "ack" {
			t.Fatalf("done-wake must not post an ack, got: %+v", m)
		}
	}
}

// Unregistered originator (human / scratch dir): done lands in the thread,
// nobody is spawned.
func TestDoneReplyNoWakeForUnregisteredOriginator(t *testing.T) {
	var spawned []SpawnConfig
	d := setupDaemon(t)
	d.Launcher = &Launcher{Binary: "/bin/echo", SpawnFn: func(cfg SpawnConfig) (int, error) {
		spawned = append(spawned, cfg)
		return 1, nil
	}}
	d.Store.db.Exec(`INSERT INTO messages(id, channel_id, thread_id, from_project, to_project, content, status, ts)
		VALUES(600, 1, NULL, 'human', 'child', 'do X', 'message', datetime('now'))`)

	done, _ := json.Marshal(map[string]any{
		"from": "child", "thread_id": 600,
		"content": "did X", "status": "done",
	})
	resp := d.Handle(context.Background(), protocol.Request{Method: "post_message", Params: done, ID: 1})
	if resp.Error != nil {
		t.Fatalf("done reply: %s", resp.Error.Message)
	}
	if len(spawned) != 0 {
		t.Fatalf("unregistered originator must not be woken, got %d spawns", len(spawned))
	}
}

// Idle-but-alive parent: resumed opencode sessions sit alive after their
// turn ends. Done-wake must SIGTERM that process before resuming, or two
// `opencode run -s` share one session.
func TestDoneWakeKillsIdleParent(t *testing.T) {
	var spawned []SpawnConfig
	d := setupDaemon(t)
	d.Launcher = &Launcher{Binary: "/bin/echo", SpawnFn: func(cfg SpawnConfig) (int, error) {
		spawned = append(spawned, cfg)
		return 1, nil
	}}
	aDir := t.TempDir()
	os.WriteFile(filepath.Join(aDir, "mouse.yaml"),
		[]byte("agent:\n  primary:\n    provider: opencode\na2a:\n  allow_inbound: true\n  allow_outbound: true\n"), 0644)
	d.Registry.AddProject("parent", aDir)
	bDir := t.TempDir()
	os.WriteFile(filepath.Join(bDir, "mouse.yaml"),
		[]byte("agent:\n  primary:\n    provider: opencode\na2a:\n  allow_inbound: true\n"), 0644)
	d.Registry.AddProject("child", bDir)

	// Real live process standing in for an idle parent agent.
	sleep := exec.Command("sleep", "30")
	if err := sleep.Start(); err != nil {
		t.Skip("cannot start sleep:", err)
	}
	defer sleep.Process.Kill()

	d.Store.db.Exec(`INSERT INTO messages(id, channel_id, thread_id, from_project, to_project, content, status, ts)
		VALUES(650, 1, NULL, 'parent', 'child', 'do X', 'message', datetime('now'))`)
	d.Store.db.Exec(`INSERT INTO sessions(project_id, agent_binary, model, status, pid, created_at, task_msg_id, root_thread_id, opencode_session_id)
		VALUES((SELECT id FROM projects WHERE name='parent'), 'opencode', 'default', 'active', ?, datetime('now'), 649, 649, 'ses_parent_idle')`,
		sleep.Process.Pid)

	done, _ := json.Marshal(map[string]any{
		"from": "child", "thread_id": 650, "content": "did X", "status": "done",
	})
	if resp := d.Handle(context.Background(), protocol.Request{Method: "post_message", Params: done, ID: 1}); resp.Error != nil {
		t.Fatalf("done reply: %s", resp.Error.Message)
	}
	if len(spawned) != 1 {
		t.Fatalf("want 1 parent wake spawn, got %d", len(spawned))
	}
	if spawned[0].AgentSessionID != "ses_parent_idle" {
		t.Fatalf("resume target = %q, want ses_parent_idle", spawned[0].AgentSessionID)
	}
	// The idle process must be gone.
	done2 := make(chan error, 1)
	go func() { _, err := sleep.Process.Wait(); done2 <- err }()
	select {
	case <-done2:
	case <-time.After(5 * time.Second):
		t.Fatal("idle parent process was not killed")
	}
}

// Finding 1 regression: killIdleParent SIGTERMs the launcher-spawned parent
// process, so the real launcher's exit watcher fires OnExit(-1). That must
// NOT post a false BLOCKED on the parent's upper thread or flip the row to
// "failed" — a daemon-killed session is not a failure.
func TestDoneWakeKillSuppressesExitWatcherBlocked(t *testing.T) {
	sleep := exec.Command("sleep", "30")
	if err := sleep.Start(); err != nil {
		t.Skip("cannot start sleep:", err)
	}
	defer sleep.Process.Kill()

	var cfgParent SpawnConfig
	d := setupDaemon(t)
	d.SafetyNetEnabled = true
	d.Launcher = &Launcher{Binary: "/bin/echo", SpawnFn: func(cfg SpawnConfig) (int, error) {
		// Fresh parent spawn = the idle parent session the daemon will
		// SIGTERM; capture its OnExit to simulate the launcher's exit
		// watcher. Resume spawns carry AgentSessionID — skip those.
		if cfg.ProjectID == "parent" && cfg.AgentSessionID == "" {
			cfgParent = cfg
			return sleep.Process.Pid, nil
		}
		return 0, nil
	}}
	aDir := t.TempDir()
	os.WriteFile(filepath.Join(aDir, "mouse.yaml"),
		[]byte("agent:\n  primary:\n    provider: opencode\na2a:\n  allow_inbound: true\n  allow_outbound: true\n"), 0644)
	d.Registry.AddProject("parent", aDir)
	bDir := t.TempDir()
	os.WriteFile(filepath.Join(bDir, "mouse.yaml"),
		[]byte("agent:\n  primary:\n    provider: opencode\na2a:\n  allow_inbound: true\n"), 0644)
	d.Registry.AddProject("child", bDir)

	// (1) Task root: orchestrator → parent. wakeAgent spawns the parent.
	root, _ := json.Marshal(map[string]any{
		"from": "orchestrator", "to": "parent", "content": "orchestrate X",
	})
	resp := d.Handle(context.Background(), protocol.Request{Method: "post_message", Params: root, ID: 1})
	if resp.Error != nil {
		t.Fatalf("root post: %s", resp.Error.Message)
	}
	var rootRes PostResult
	json.Unmarshal(resp.Result, &rootRes)
	if rootRes.MessageID == 0 {
		t.Fatal("no root message id")
	}
	if cfgParent.OnExit == nil {
		t.Fatal("parent was not spawned via SpawnFn")
	}
	d.Store.db.Exec(`UPDATE sessions SET opencode_session_id='ses_parent'
		WHERE project_id=(SELECT id FROM projects WHERE name='parent')`)

	// (2) Parent posts a sub-task root → child gets spawned.
	sub, _ := json.Marshal(map[string]any{
		"from": "parent", "to": "child", "content": "do X",
	})
	resp = d.Handle(context.Background(), protocol.Request{Method: "post_message", Params: sub, ID: 2})
	if resp.Error != nil {
		t.Fatalf("sub-task post: %s", resp.Error.Message)
	}
	var pr PostResult
	json.Unmarshal(resp.Result, &pr)
	if pr.MessageID == 0 {
		t.Fatal("no sub-task message id")
	}

	// (3) Child posts done on the sub-task → daemon kills the idle parent.
	done, _ := json.Marshal(map[string]any{
		"from": "child", "thread_id": pr.MessageID,
		"content": "did X", "status": "done",
	})
	resp = d.Handle(context.Background(), protocol.Request{Method: "post_message", Params: done, ID: 3})
	if resp.Error != nil {
		t.Fatalf("done reply: %s", resp.Error.Message)
	}

	// (4) Simulate the launcher's exit watcher on the killed parent process.
	cfgParent.OnExit(-1)

	// (5) Original parent session still "exited" (not flipped to "failed").
	var status string
	err := d.Store.db.QueryRow(
		`SELECT status FROM sessions
		 WHERE project_id=(SELECT id FROM projects WHERE name='parent')
		 ORDER BY id ASC LIMIT 1`).Scan(&status)
	if err != nil {
		t.Fatal(err)
	}
	if status != "exited" {
		t.Fatalf("killed parent session status = %q, want exited (not failed)", status)
	}
	// And the parent's upper thread carries no false BLOCKED.
	msgs, err := d.Comms.ReadThread(rootRes.MessageID)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		if strings.HasPrefix(m.Content, "BLOCKED") {
			t.Fatalf("false BLOCKED on parent's upper thread: %+v", m)
		}
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
	userDir := t.TempDir()
	os.WriteFile(filepath.Join(userDir, "mouse.yaml"),
		[]byte("agent:\n  primary:\n    provider: opencode\na2a:\n  allow_inbound: true\n"), 0644)
	d.Registry.AddProject("user-service", userDir)

	// Seed: thread root 500, an exited session bound to it, and a done reply.
	d.Store.db.Exec(`INSERT INTO messages(id, channel_id, thread_id, from_project, to_project, content, status, ts)
		VALUES(500, 1, NULL, 'payment', 'user-service', 'task', 'message', datetime('now'))`)
	d.Store.db.Exec(`INSERT INTO sessions(project_id, agent_binary, model, status, pid, created_at, task_msg_id, root_thread_id)
		VALUES((SELECT id FROM projects WHERE name='user-service'), 'opencode', 'default', 'exited', 0, datetime('now'), 500, 500)`)
	d.Store.db.Exec(`INSERT INTO messages(channel_id, thread_id, from_project, to_project, content, status, ts)
		VALUES(1, 500, 'user-service', 'payment', 'done', 'done', datetime('now'))`)

	// Follow-up reply with to= — SHOULD wake (resume path). Session is
	// exited, not active; done reply doesn't block re-wake anymore.
	params, _ := json.Marshal(map[string]any{
		"channel": 1, "thread_id": 500, "from": "payment",
		"to": "user-service", "content": "follow-up",
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
	userDir := t.TempDir()
	os.WriteFile(filepath.Join(userDir, "mouse.yaml"),
		[]byte("agent:\n  primary:\n    provider: opencode\na2a:\n  allow_inbound: true\n"), 0644)
	d.Registry.AddProject("user-service", userDir)

	// Seed: thread root 500 with an ACTIVE session bound. No done reply —
	// the done-reply guard won't fire; only the active-session guard should
	// suppress the wake here.
	d.Store.db.Exec(`INSERT INTO messages(id, channel_id, thread_id, from_project, to_project, content, status, ts)
		VALUES(500, 1, NULL, 'payment', 'user-service', 'task', 'message', datetime('now'))`)
	d.Store.db.Exec(`INSERT INTO sessions(project_id, agent_binary, model, status, pid, created_at, task_msg_id, root_thread_id)
		VALUES((SELECT id FROM projects WHERE name='user-service'), 'opencode', 'default', 'active', 0, datetime('now'), 500, 500)`)

	params, _ := json.Marshal(map[string]any{
		"channel": 1, "thread_id": 500, "from": "payment",
		"to": "user-service", "content": "follow-up",
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

func TestWakeAgent_AlwaysFreshSpawn(t *testing.T) {
	d := setupDaemon(t)
	// Stub the launcher's Spawn to record args without running opencode.
	var spawnArgs []string
	d.Launcher = &Launcher{Binary: "/bin/echo", SpawnFn: func(cfg SpawnConfig) (int, error) {
		spawnArgs = append(spawnArgs, cfg.AgentSessionID)
		return 1, nil
	}}

	userDir := t.TempDir()
	os.WriteFile(filepath.Join(userDir, "mouse.yaml"),
		[]byte("agent:\n  primary:\n    provider: opencode\na2a:\n  allow_inbound: true\n"), 0644)
	d.Registry.AddProject("user-service", userDir)

	// First wake: thread root, fresh spawn, captures OC ID ses_fresh1.
	p1, _ := json.Marshal(map[string]any{"from": "payment", "to": "user-service", "content": "task 1"})
	r1 := d.Handle(context.Background(), protocol.Request{Method: "post_message", Params: p1, ID: 1})
	var pr1 PostResult
	json.Unmarshal(r1.Result, &pr1)

	// Mark the first session exited so the wake guard allows a follow-up.
	d.Store.db.Exec(`UPDATE sessions SET status='exited' WHERE task_msg_id=?`, pr1.MessageID)

	// Second wake: reply on the same thread, same project — fresh spawn
	// (resume disabled: opencode run -s doesn't exit). Both spawns are
	// fresh; capturer called twice.
	p2, _ := json.Marshal(map[string]any{
		"channel": 1, "thread_id": pr1.MessageID, "from": "payment",
		"to": "user-service", "content": "follow-up",
	})
	r2 := d.Handle(context.Background(), protocol.Request{Method: "post_message", Params: p2, ID: 2})
	if r2.Error != nil {
		t.Fatalf("second post: %s", r2.Error.Message)
	}

	if len(spawnArgs) != 2 {
		t.Fatalf("expected 2 spawns, got %d", len(spawnArgs))
	}
	// Both spawns fresh — no AgentSessionID (resume disabled, no capture).
	if spawnArgs[0] != "" {
		t.Errorf("first spawn: AgentSessionID=%q want empty (fresh)", spawnArgs[0])
	}
	if spawnArgs[1] != "" {
		t.Errorf("second spawn: AgentSessionID=%q want empty (fresh)", spawnArgs[1])
	}
}

func TestWakeAgent_PrefixGeneratedOnceAndInherited(t *testing.T) {
	d := setupDaemon(t)
	userDir := t.TempDir()
	os.WriteFile(filepath.Join(userDir, "mouse.yaml"),
		[]byte("agent:\n  primary:\n    provider: opencode\na2a:\n  allow_inbound: true\n"), 0644)
	d.Registry.AddProject("user-service", userDir)

	// First wake: generates a prefix.
	p1, _ := json.Marshal(map[string]any{"from": "payment", "to": "user-service", "content": "t1"})
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
		"channel": 1, "thread_id": pr1.MessageID, "from": "payment",
		"to": "user-service", "content": "t2",
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

func TestDaemon_ResolveToProject(t *testing.T) {
	d := setupDaemon(t)
	if _, err := d.Registry.AddProject("payment", "/tmp/payA"); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		to      string
		want    string
		wantErr string
	}{
		// A slash is a legacy "workspace/project" identity — a clear error,
		// not a silent split.
		{"slash fails resolution", "co/payment", "", "bare project name"},
		{"registered bare name passes", "payment", "payment", ""},
		{"unregistered bare name passes (mailbox)", "ghost", "ghost", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := d.resolveToProject(tt.to)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("want error containing %q, got nil (resolved to %q)", tt.wantErr, got)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("want error containing %q, got %q", tt.wantErr, err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %q want %q", got, tt.want)
			}
		})
	}
}

// Ambiguity is gone with UNIQUE(name); the resolution failure that remains is
// a legacy "ws/name" identity in `to` — it must error, not silently split.
func TestHandlePost_SlashToErrors(t *testing.T) {
	d := setupDaemon(t)
	if _, err := d.Registry.AddProject("payment", "/tmp/payA"); err != nil {
		t.Fatal(err)
	}

	params, _ := json.Marshal(PostParams{From: "me", To: "co/payment", Content: "hi"})
	resp := d.handlePost(context.Background(), protocol.Request{Method: "post_message", Params: params, ID: 1})
	if resp.Error == nil {
		t.Fatal("expected error for legacy slash `to`")
	}
	if !strings.Contains(resp.Error.Message, "bare project name") {
		t.Fatalf("want error containing 'bare project name', got %q", resp.Error.Message)
	}
}

// ============================================
// Todos — RPC handler tests
// ============================================

func TestHandle_TodoAddUpdateList(t *testing.T) {
	d := setupDaemon(t)
	d.Store.db.Exec(`INSERT INTO channels(id, name, type) VALUES(10, 'dm', 'dm')`)
	postParams, _ := json.Marshal(map[string]any{
		"channel": 10, "from": "payment", "content": "task root",
	})
	resp := d.Handle(context.Background(), protocol.Request{Method: "post_message", Params: postParams, ID: 1})
	if resp.Error != nil {
		t.Fatal(resp.Error.Message)
	}
	var pr PostResult
	json.Unmarshal(resp.Result, &pr)

	addParams, _ := json.Marshal(map[string]any{"thread_id": pr.MessageID, "content": "step one"})
	resp = d.Handle(context.Background(), protocol.Request{Method: "todo_add", Params: addParams, ID: 2})
	if resp.Error != nil {
		t.Fatal(resp.Error.Message)
	}
	var added struct {
		ID int64 `json:"id"`
	}
	json.Unmarshal(resp.Result, &added)
	if added.ID == 0 {
		t.Fatalf("todo_add: got %+v", added)
	}

	updParams, _ := json.Marshal(map[string]any{"id": added.ID, "state": "done"})
	resp = d.Handle(context.Background(), protocol.Request{Method: "todo_update", Params: updParams, ID: 3})
	if resp.Error != nil {
		t.Fatal(resp.Error.Message)
	}

	listParams, _ := json.Marshal(map[string]any{"thread_id": pr.MessageID})
	resp = d.Handle(context.Background(), protocol.Request{Method: "todo_list", Params: listParams, ID: 4})
	if resp.Error != nil {
		t.Fatal(resp.Error.Message)
	}
	var todos []Todo
	json.Unmarshal(resp.Result, &todos)
	if len(todos) != 1 || todos[0].State != "done" {
		t.Fatalf("todo_list: got %+v", todos)
	}
}

func TestHandle_TodoAdd_NonexistentThreadErrors(t *testing.T) {
	d := setupDaemon(t)
	addParams, _ := json.Marshal(map[string]any{"thread_id": 999, "content": "x"})
	resp := d.Handle(context.Background(), protocol.Request{Method: "todo_add", Params: addParams, ID: 1})
	if resp.Error == nil {
		t.Fatal("expected error for todo_add on nonexistent thread")
	}
}

func TestHandle_TodoUpdate_BadStateErrors(t *testing.T) {
	d := setupDaemon(t)
	d.Store.db.Exec(`INSERT INTO channels(id, name, type) VALUES(10, 'dm', 'dm')`)
	postParams, _ := json.Marshal(map[string]any{"channel": 10, "from": "a", "content": "root"})
	resp := d.Handle(context.Background(), protocol.Request{Method: "post_message", Params: postParams, ID: 1})
	var pr PostResult
	json.Unmarshal(resp.Result, &pr)
	addParams, _ := json.Marshal(map[string]any{"thread_id": pr.MessageID, "content": "step"})
	resp = d.Handle(context.Background(), protocol.Request{Method: "todo_add", Params: addParams, ID: 2})
	var added struct {
		ID int64 `json:"id"`
	}
	json.Unmarshal(resp.Result, &added)

	updParams, _ := json.Marshal(map[string]any{"id": added.ID, "state": "bogus"})
	resp = d.Handle(context.Background(), protocol.Request{Method: "todo_update", Params: updParams, ID: 3})
	if resp.Error == nil {
		t.Fatal("expected error for bad state")
	}
}

func TestHandle_TodoThreads(t *testing.T) {
	d := setupDaemon(t)
	d.Store.db.Exec(`INSERT INTO channels(id, name, type) VALUES(10, 'dm', 'dm')`)
	postParams, _ := json.Marshal(map[string]any{"channel": 10, "from": "a", "content": "add payment_status field to User"})
	resp := d.Handle(context.Background(), protocol.Request{Method: "post_message", Params: postParams, ID: 1})
	var pr PostResult
	json.Unmarshal(resp.Result, &pr)
	addParams, _ := json.Marshal(map[string]any{"thread_id": pr.MessageID, "content": "step one"})
	d.Handle(context.Background(), protocol.Request{Method: "todo_add", Params: addParams, ID: 2})

	resp = d.Handle(context.Background(), protocol.Request{Method: "todo_threads", Params: nil, ID: 3})
	if resp.Error != nil {
		t.Fatal(resp.Error.Message)
	}
	var rows []struct {
		ThreadID int64  `json:"thread_id"`
		Preview  string `json:"preview"`
		Done     int    `json:"done"`
		Total    int    `json:"total"`
	}
	json.Unmarshal(resp.Result, &rows)
	if len(rows) != 1 || rows[0].ThreadID != pr.MessageID || rows[0].Total != 1 || rows[0].Done != 0 {
		t.Fatalf("todo_threads: got %+v", rows)
	}
}

// TestWakeAgent_ResumeScopedToBinary guards against a fallback runtime switch
// resuming a foreign runtime's session id (found in final review: the resume
// lookup used to ignore agent_binary entirely).
func TestWakeAgent_ResumeScopedToBinary(t *testing.T) {
	d := setupDaemon(t)
	userDir := t.TempDir()
	os.WriteFile(filepath.Join(userDir, "mouse.yaml"),
		[]byte("agent:\n  primary:\n    provider: opencode\n  secondary:\n    provider: claude\na2a:\n  allow_inbound: true\n"), 0644)
	d.Registry.AddProject("user-service", userDir)

	// First wake: primary (opencode) available, captures a fake opencode session id.
	d.LookPath = func(string) (string, error) { return "/usr/bin/x", nil }
	d.CaptureAgentSessionID = func(SpawnConfig) (string, error) { return "ses_opencode123", nil }
	params, _ := json.Marshal(map[string]any{
		"from": "payment", "to": "user-service", "content": "task 1",
	})
	resp := d.Handle(context.Background(), protocol.Request{Method: "post_message", Params: params, ID: 1})
	var pr PostResult
	json.Unmarshal(resp.Result, &pr)

	// captureAgentSessionID runs async (goroutine); wait for the opencode
	// session row to be stamped before the second wake queries it.
	deadline := time.Now().Add(2 * time.Second)
	for {
		var ocID sql.NullString
		d.Store.db.QueryRow(`SELECT opencode_session_id FROM sessions WHERE task_msg_id=?`, pr.MessageID).Scan(&ocID)
		if ocID.String == "ses_opencode123" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for opencode session id to be captured")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Second wake on the same thread: opencode binary now unavailable, forces
	// fallback to secondary (claude). The resume lookup must NOT hand the
	// opencode session id to the claude spawn.
	d.LookPath = func(bin string) (string, error) {
		if bin == "opencode" {
			return "", os.ErrNotExist
		}
		return "/usr/bin/" + bin, nil
	}
	var spawnedSessionID string
	d.Launcher = &Launcher{SpawnFn: func(cfg SpawnConfig) (int, error) {
		spawnedSessionID = cfg.AgentSessionID
		return 1, nil
	}}
	params2, _ := json.Marshal(map[string]any{
		"thread_id": pr.MessageID, "from": "payment", "to": "user-service", "content": "follow-up",
	})
	resp = d.Handle(context.Background(), protocol.Request{Method: "post_message", Params: params2, ID: 2})
	if resp.Error != nil {
		t.Fatal(resp.Error.Message)
	}
	if spawnedSessionID == "ses_opencode123" {
		t.Fatalf("claude spawn resumed opencode's session id %q — resume must be scoped to agent_binary", spawnedSessionID)
	}
	if spawnedSessionID != "" {
		t.Errorf("expected no resume id for claude's first spawn on this thread, got %q", spawnedSessionID)
	}
}

// Second project on a thread must not resume the first project's session id.
func TestWakeAgent_ResumeScopedToProject(t *testing.T) {
	d := setupDaemon(t)
	mouse := []byte("agent:\n  primary:\n    provider: opencode\na2a:\n  allow_inbound: true\n")
	dirA, dirB := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(dirA, "mouse.yaml"), mouse, 0644)
	os.WriteFile(filepath.Join(dirB, "mouse.yaml"), mouse, 0644)
	d.Registry.AddProject("proj-a", dirA)
	d.Registry.AddProject("proj-b", dirB)

	d.LookPath = func(string) (string, error) { return "/usr/bin/x", nil }
	d.CaptureAgentSessionID = func(SpawnConfig) (string, error) { return "ses_projA123", nil }
	params, _ := json.Marshal(map[string]any{
		"from": "orchestrator", "to": "proj-a", "content": "task for A",
	})
	resp := d.Handle(context.Background(), protocol.Request{Method: "post_message", Params: params, ID: 1})
	var pr PostResult
	json.Unmarshal(resp.Result, &pr)

	deadline := time.Now().Add(2 * time.Second)
	for {
		var ocID sql.NullString
		d.Store.db.QueryRow(`SELECT opencode_session_id FROM sessions WHERE task_msg_id=?`, pr.MessageID).Scan(&ocID)
		if ocID.String == "ses_projA123" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for proj-a session id capture")
		}
		time.Sleep(10 * time.Millisecond)
	}

	var spawnedSessionID, spawnedDir string
	d.Launcher = &Launcher{SpawnFn: func(cfg SpawnConfig) (int, error) {
		spawnedSessionID, spawnedDir = cfg.AgentSessionID, cfg.Dir
		return 1, nil
	}}
	params2, _ := json.Marshal(map[string]any{
		"thread_id": pr.MessageID, "from": "orchestrator", "to": "proj-b", "content": "task for B",
	})
	resp = d.Handle(context.Background(), protocol.Request{Method: "post_message", Params: params2, ID: 2})
	if resp.Error != nil {
		t.Fatal(resp.Error.Message)
	}
	if spawnedDir != dirB {
		t.Fatalf("second wake spawned in %q, want proj-b dir %q", spawnedDir, dirB)
	}
	if spawnedSessionID != "" {
		t.Fatalf("proj-b resumed %q — resume must be scoped to project_id", spawnedSessionID)
	}
}

// The next_action wording must match the push-wake contract: working says
// "you will be woken", terminal says what to do with the result.
func TestNextAction_TellsCallerWhetherToStop(t *testing.T) {
	cases := []struct {
		name        string
		hasDone     bool
		agentStatus string
		wantStop    bool
	}{
		{"done", true, "exited", true},
		{"done while process still alive", true, "working", true},
		{"exited without a done reply", false, "exited", true},
		{"failed without a done reply", false, "failed", true},
		{"never woke", false, "no_agent", true},
		{"still running", false, "working", false},
	}
	for _, c := range cases {
		got := nextAction(c.hasDone, c.agentStatus, 42)
		if got == "" {
			t.Fatalf("%s: empty next_action", c.name)
		}
		isStillWorking := strings.HasPrefix(got, "agent still working")
		if c.wantStop && isStillWorking {
			t.Errorf("%s: want a stop instruction, got %q", c.name, got)
		}
		if !c.wantStop && !isStillWorking {
			t.Errorf("%s: want still-working guidance, got %q", c.name, got)
		}
	}
}

// A child runs in its own process and can't write into the caller's session,
// so task_status is the only window into it: it must carry the child's work
// items, current step, and latest reply — not just a done flag.
func TestTaskStatus_CarriesChildProgressDetail(t *testing.T) {
	d := setupDaemon(t)
	userDir := t.TempDir()
	os.WriteFile(filepath.Join(userDir, "mouse.yaml"),
		[]byte("agent:\n  primary:\n    provider: opencode\na2a:\n  allow_inbound: true\n"), 0644)
	d.Registry.AddProject("user-service", userDir)

	d.Store.db.Exec(`INSERT INTO messages(id, channel_id, thread_id, from_project, to_project, content, status, ts)
		VALUES(700, 1, NULL, 'payment', 'user-service', 'task', 'message', datetime('now'))`)
	d.Store.db.Exec(`INSERT INTO sessions(project_id, agent_binary, model, status, pid, created_at, task_msg_id, root_thread_id)
		VALUES((SELECT id FROM projects WHERE name='user-service'), 'opencode', 'default', 'active', 0, datetime('now'), 700, 700)`)
	// The child plans three steps and finishes the first.
	first, _ := d.Todos.Add(700, "write migration")
	d.Todos.Add(700, "update handler")
	d.Todos.Add(700, "run tests")
	d.Todos.Update(first.ID, "done")
	d.Store.db.Exec(`INSERT INTO messages(channel_id, thread_id, from_project, content, status, ts)
		VALUES(1, 700, 'user-service', 'migration written, moving on' || char(10) || 'second line ignored', 'message', datetime('now'))`)

	params, _ := json.Marshal(map[string]any{"message_id": 700})
	resp := d.Handle(context.Background(), protocol.Request{Method: "task_status", Params: params, ID: 1})
	if resp.Error != nil {
		t.Fatalf("task_status: %s", resp.Error.Message)
	}
	var ts TaskStatusResult
	json.Unmarshal(resp.Result, &ts)

	if ts.TodosDone != 1 || ts.TodosTotal != 3 {
		t.Errorf("todos: got %d/%d want 1/3", ts.TodosDone, ts.TodosTotal)
	}
	if ts.CurrentStep != "update handler" {
		t.Errorf("current_step: got %q want %q", ts.CurrentStep, "update handler")
	}
	if len(ts.Todos) != 3 {
		t.Errorf("todos list: got %d entries want 3", len(ts.Todos))
	}
	if ts.Project != "user-service" {
		t.Errorf("project: got %q", ts.Project)
	}
	if ts.LastUpdate != "migration written, moving on" {
		t.Errorf("last_update should be the first line only, got %q", ts.LastUpdate)
	}
}

// A2A consent is two-sided: the recipient declares allow_inbound, the sender
// declares allow_outbound. Only inbound was enforced — allow_outbound existed
// in mouse.yaml but nothing read it, so a repo that forbade delegating out
// still could.
func TestHandle_Post_OutboundDenied(t *testing.T) {
	const inbound = "agent:\n  primary:\n    provider: opencode\na2a:\n  allow_inbound: true\n  allow_outbound: true\n"

	newDaemon := func(t *testing.T, senderYAML string) *Daemon {
		d := setupDaemon(t)
		senderDir := t.TempDir()
		os.WriteFile(filepath.Join(senderDir, "mouse.yaml"), []byte(senderYAML), 0644)
		d.Registry.AddProject("payment", senderDir)
		recvDir := t.TempDir()
		os.WriteFile(filepath.Join(recvDir, "mouse.yaml"), []byte(inbound), 0644)
		d.Registry.AddProject("user-service", recvDir)
		return d
	}
	post := func(d *Daemon) protocol.Response {
		params, _ := json.Marshal(map[string]any{
			"from": "payment", "to": "user-service", "content": "do X"})
		return d.Handle(context.Background(), protocol.Request{Method: "post_message", Params: params, ID: 1})
	}

	t.Run("explicitly denied", func(t *testing.T) {
		d := newDaemon(t, "agent:\n  primary:\n    provider: opencode\na2a:\n  allow_inbound: true\n  allow_outbound: false\n")
		if resp := post(d); resp.Error == nil {
			t.Fatal("expected outbound-denied error")
		} else if !strings.Contains(resp.Error.Message, "outbound") {
			t.Fatalf("error should name outbound consent, got %q", resp.Error.Message)
		}
	})

	// Absent field is false in Go — same deny-by-default as inbound already has.
	t.Run("field absent", func(t *testing.T) {
		d := newDaemon(t, "agent:\n  primary:\n    provider: opencode\na2a:\n  allow_inbound: true\n")
		if resp := post(d); resp.Error == nil {
			t.Fatal("absent allow_outbound should deny, matching inbound's behaviour")
		}
	})

	t.Run("allowed", func(t *testing.T) {
		d := newDaemon(t, inbound)
		if resp := post(d); resp.Error != nil {
			t.Fatalf("allow_outbound:true must permit the wake, got %q", resp.Error.Message)
		}
	})

	// A human orchestrating from an unregistered dir has no mouse.yaml and so
	// declares nothing — must not be governed by a repo's outbound rule.
	t.Run("unregistered sender unaffected", func(t *testing.T) {
		d := newDaemon(t, "agent:\n  primary:\n    provider: opencode\na2a:\n  allow_inbound: true\n  allow_outbound: false\n")
		params, _ := json.Marshal(map[string]any{
			"from": "some/scratch-dir", "to": "user-service", "content": "do X"})
		if resp := d.Handle(context.Background(), protocol.Request{Method: "post_message", Params: params, ID: 1}); resp.Error != nil {
			t.Fatalf("unregistered sender should be unrestricted, got %q", resp.Error.Message)
		}
	})
}

// A thread opened from an unregistered directory carries a non-project
// identity (e.g. "dir:ledger"). Auto-filling that as a reply's recipient
// would fail the post, since it can't resolve to a registered project.
func TestHandle_ReplyDoesNotAutoFillNonProjectSender(t *testing.T) {
	d := setupDaemon(t)
	userDir := t.TempDir()
	os.WriteFile(filepath.Join(userDir, "mouse.yaml"),
		[]byte("agent:\n  primary:\n    provider: opencode\na2a:\n  allow_inbound: true\n  allow_outbound: true\n"), 0644)
	d.Registry.AddProject("user-service", userDir)

	// Root dispatched from a plain directory, not a registered project.
	d.Store.db.Exec(`INSERT INTO messages(id, channel_id, thread_id, from_project, to_project, content, status, ts)
		VALUES(800, 1, NULL, 'dir:ledger', 'user-service', 'task', 'message', datetime('now'))`)

	params, _ := json.Marshal(map[string]any{
		"channel": 1, "thread_id": 800, "from": "user-service",
		"status": "in_progress", "content": "progress note",
	})
	resp := d.Handle(context.Background(), protocol.Request{Method: "post_message", Params: params, ID: 1})
	if resp.Error != nil {
		t.Fatalf("reply should post cleanly, got %q", resp.Error.Message)
	}
	var to string
	d.Store.db.QueryRow(`SELECT IFNULL(to_project,'') FROM messages WHERE thread_id=800`).Scan(&to)
	if to != "" {
		t.Errorf("to_project = %q; a non-project sender must not be auto-filled", to)
	}
}

// Two projects working one parent task: the wake guard must be scoped to the
// project being addressed. A thread-wide guard silently swallowed the second
// project's spawn, which forced every delegation onto its own root thread and
// left related work looking like unrelated parents.
func TestHandle_SecondProjectOnSameThreadStillWakes(t *testing.T) {
	d := setupDaemon(t)
	const yaml = "agent:\n  primary:\n    provider: opencode\na2a:\n  allow_inbound: true\n  allow_outbound: true\n"
	for _, name := range []string{"backend", "frontend"} {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "mouse.yaml"), []byte(yaml), 0644)
		d.Registry.AddProject(name, dir)
	}

	// Root thread 900 with backend already working on it.
	d.Store.db.Exec(`INSERT INTO messages(id, channel_id, thread_id, from_project, to_project, content, status, ts)
		VALUES(900, 1, NULL, 'dir:lab', 'backend', 'do the backend half', 'message', datetime('now'))`)
	d.Store.db.Exec(`INSERT INTO sessions(project_id, agent_binary, model, status, pid, created_at, task_msg_id, root_thread_id)
		VALUES((SELECT id FROM projects WHERE name='backend'), 'opencode', 'default', 'active', 0, datetime('now'), 900, 900)`)

	// The other half of the same task, on the same thread.
	params, _ := json.Marshal(map[string]any{
		"channel": 1, "thread_id": 900, "from": "dir:lab",
		"to": "frontend", "content": "do the frontend half",
	})
	if resp := d.Handle(context.Background(), protocol.Request{Method: "post_message", Params: params, ID: 1}); resp.Error != nil {
		t.Fatalf("post: %s", resp.Error.Message)
	}
	var frontendSessions int
	d.Store.db.QueryRow(`SELECT count(*) FROM sessions
		WHERE root_thread_id=900 AND project_id=(SELECT id FROM projects WHERE name='frontend')`).Scan(&frontendSessions)
	if frontendSessions != 1 {
		t.Fatalf("frontend sessions on thread 900 = %d, want 1 — a busy sibling project must not suppress its wake", frontendSessions)
	}

	// Same project twice while it is working is still suppressed: that guard
	// exists to stop duplicate spawns, and must survive the rescoping.
	dup, _ := json.Marshal(map[string]any{
		"channel": 1, "thread_id": 900, "from": "dir:lab",
		"to": "backend", "content": "nudge",
	})
	d.Handle(context.Background(), protocol.Request{Method: "post_message", Params: dup, ID: 2})
	var backendSessions int
	d.Store.db.QueryRow(`SELECT count(*) FROM sessions
		WHERE root_thread_id=900 AND project_id=(SELECT id FROM projects WHERE name='backend')`).Scan(&backendSessions)
	if backendSessions != 1 {
		t.Errorf("backend sessions = %d, want 1 — a project already working must not be re-spawned", backendSessions)
	}
}

// TestHandle_AckReplyOnSpawn: the daemon posts a pickup notice as soon as the
// agent process starts, so a caller can tell "spawned and quiet" from "never
// spawned" without waiting on the agent to say anything. The ack must not
// register as completion.
func TestHandle_AckReplyOnSpawn(t *testing.T) {
	d := setupDaemon(t)
	userDir := t.TempDir()
	os.WriteFile(filepath.Join(userDir, "mouse.yaml"), []byte("agent:\n  primary:\n    provider: opencode\na2a:\n  allow_inbound: true\n"), 0644)
	d.Registry.AddProject("user-service", userDir)

	params, _ := json.Marshal(map[string]any{
		"from":    "payment-service",
		"to":      "user-service",
		"content": "add field payment_status",
	})
	resp := d.Handle(context.Background(), protocol.Request{Method: "post_message", Params: params, ID: 1})
	if resp.Error != nil {
		t.Fatalf("post failed: %s", resp.Error.Message)
	}
	var pr PostResult
	json.Unmarshal(resp.Result, &pr)

	var content, from string
	err := d.Store.db.QueryRow(
		`SELECT content, from_project FROM messages WHERE thread_id=? AND status='ack'`,
		pr.MessageID).Scan(&content, &from)
	if err != nil {
		t.Fatalf("no ack reply on thread %d: %v", pr.MessageID, err)
	}
	if !strings.Contains(content, "working on it") {
		t.Errorf("ack content = %q, want a pickup notice", content)
	}
	if from != "user-service" {
		t.Errorf("ack from = %q, want the project doing the work", from)
	}

	// The ack is aimed at whoever dispatched, not at the lobby: visible in
	// read_thread, filtered out of read_channel.
	rt, _ := json.Marshal(map[string]any{"message_id": pr.MessageID})
	var thread []Message
	json.Unmarshal(d.Handle(context.Background(), protocol.Request{Method: "read_thread", Params: rt, ID: 10}).Result, &thread)
	if !hasStatus(thread, "ack") {
		t.Error("read_thread must show the ack — it is the pickup signal")
	}
	rc, _ := json.Marshal(map[string]any{})
	var channel []Message
	json.Unmarshal(d.Handle(context.Background(), protocol.Request{Method: "read_channel", Params: rc, ID: 11}).Result, &channel)
	if hasStatus(channel, "ack") {
		t.Error("read_channel must not carry acks — one per spawn would flood the lobby")
	}

	// An ack is not a done reply — task_status must still report incomplete.
	sp, _ := json.Marshal(map[string]any{"message_id": pr.MessageID})
	sresp := d.Handle(context.Background(), protocol.Request{Method: "task_status", Params: sp, ID: 2})
	var st TaskStatusResult
	json.Unmarshal(sresp.Result, &st)
	if st.HasDone {
		t.Error("ack reply counted as completion")
	}
	if st.LastUpdate == "" {
		t.Error("ack should surface as last_update so the caller sees pickup immediately")
	}
}

// TestHandle_ReportProgress: a child's status update reaches the parent via
// task_status, carries its ETA, and never spawns anyone to deliver it.
func TestHandle_ReportProgress(t *testing.T) {
	d := setupDaemon(t)
	userDir := t.TempDir()
	os.WriteFile(filepath.Join(userDir, "mouse.yaml"), []byte("agent:\n  primary:\n    provider: opencode\na2a:\n  allow_inbound: true\n"), 0644)
	d.Registry.AddProject("user-service", userDir)
	// Parent is registered and wakeable, so "no spawn" below means the
	// progress report chose not to wake it, not that it couldn't.
	payDir := t.TempDir()
	os.WriteFile(filepath.Join(payDir, "mouse.yaml"), []byte("agent:\n  primary:\n    provider: opencode\na2a:\n  allow_inbound: true\n  allow_outbound: true\n"), 0644)
	d.Registry.AddProject("payment-service", payDir)

	params, _ := json.Marshal(map[string]any{
		"from": "payment-service", "to": "user-service",
		"content": "add field payment_status",
	})
	resp := d.Handle(context.Background(), protocol.Request{Method: "post_message", Params: params, ID: 1})
	var pr PostResult
	json.Unmarshal(resp.Result, &pr)

	var before int
	d.Store.db.QueryRow(`SELECT count(*) FROM sessions`).Scan(&before)

	rp, _ := json.Marshal(map[string]any{
		"thread_id": pr.MessageID, "from": "user-service",
		"note": "migrating the schema", "eta_minutes": 12,
	})
	presp := d.Handle(context.Background(), protocol.Request{Method: "report_progress", Params: rp, ID: 2})
	if presp.Error != nil {
		t.Fatalf("report_progress: %s", presp.Error.Message)
	}

	// Reporting progress must not wake the parent — that would be a whole
	// agent process spawned just to receive a status line.
	var after int
	d.Store.db.QueryRow(`SELECT count(*) FROM sessions`).Scan(&after)
	if after != before {
		t.Errorf("progress report spawned %d extra session(s)", after-before)
	}

	sp, _ := json.Marshal(map[string]any{"message_id": pr.MessageID})
	sresp := d.Handle(context.Background(), protocol.Request{Method: "task_status", Params: sp, ID: 3})
	var st TaskStatusResult
	json.Unmarshal(sresp.Result, &st)
	if !strings.Contains(st.ProgressNote, "migrating the schema") {
		t.Errorf("progress_note = %q, want the child's note", st.ProgressNote)
	}
	if st.ETAMinutes != 12 {
		t.Errorf("eta_minutes = %d, want 12", st.ETAMinutes)
	}
	if st.HasDone {
		t.Error("a progress report must not count as completion")
	}
}

// The child finished; a broken parent wake must not fail the child's done
// post. The failure is recorded as a BLOCKED reply for the TUI/human.
func TestDoneReplyWakeFailurePostsBlockedNotError(t *testing.T) {
	d := setupDaemon(t)
	d.Launcher = &Launcher{SpawnFn: func(cfg SpawnConfig) (int, error) {
		return 0, errors.New("opencode not installed")
	}}
	aDir := t.TempDir()
	os.WriteFile(filepath.Join(aDir, "mouse.yaml"),
		[]byte("agent:\n  primary:\n    provider: opencode\na2a:\n  allow_inbound: true\n  allow_outbound: true\n"), 0644)
	d.Registry.AddProject("parent", aDir)
	bDir := t.TempDir()
	os.WriteFile(filepath.Join(bDir, "mouse.yaml"),
		[]byte("agent:\n  primary:\n    provider: opencode\na2a:\n  allow_inbound: true\n"), 0644)
	d.Registry.AddProject("child", bDir)
	d.Store.db.Exec(`INSERT INTO messages(id, channel_id, thread_id, from_project, to_project, content, status, ts)
		VALUES(800, 1, NULL, 'parent', 'child', 'do X', 'message', datetime('now'))`)

	done, _ := json.Marshal(map[string]any{
		"from": "child", "thread_id": 800, "content": "did X", "status": "done",
	})
	resp := d.Handle(context.Background(), protocol.Request{Method: "post_message", Params: done, ID: 1})
	if resp.Error != nil {
		t.Fatalf("done post must succeed despite wake failure, got: %s", resp.Error.Message)
	}
	msgs, _ := d.Comms.ReadThread(800)
	found := false
	for _, m := range msgs {
		if strings.HasPrefix(m.Content, "BLOCKED: parent wake failed") {
			found = true
		}
	}
	if !found {
		t.Fatalf("thread missing BLOCKED wake-failure reply, got %d messages", len(msgs))
	}
}

// TUI panels poll these every 2s per client; full bodies would drown
// hmf.log. Assert quiet summary lines, no recv/send bodies.
func TestQuietMethodsLogSummary(t *testing.T) {
	d := setupDaemon(t)
	d.Store.db.Exec(`INSERT INTO projects(id, name, path) VALUES(1, 'child', '/tmp/child')`)
	d.Store.db.Exec(`INSERT INTO messages(id, channel_id, thread_id, from_project, to_project, content, status, ts)
		VALUES(500, 1, NULL, 'parent', 'child', 'do X please', 'message', datetime('now','-1 hour'))`)

	for i, method := range []string{"thread_list", "session_list"} {
		resp := d.Handle(context.Background(), protocol.Request{Method: method, ID: int64(i + 1)})
		if resp.Error != nil {
			t.Fatalf("%s: %s", method, resp.Error.Message)
		}
	}
	b, err := os.ReadFile(LogPath())
	if err != nil {
		t.Fatal(err)
	}
	log := string(b)
	for _, method := range []string{"thread_list", "session_list"} {
		if strings.Contains(log, "recv id= method="+method) || strings.Contains(log, "method="+method+" params=") {
			t.Errorf("%s logged full params", method)
		}
		if strings.Contains(log, "send id= method="+method) || strings.Contains(log, "method="+method+" result=") {
			t.Errorf("%s logged full result", method)
		}
		if !strings.Contains(log, "method="+method+" dur=") {
			t.Errorf("%s missing quiet summary line", method)
		}
	}
}

func TestThreadList(t *testing.T) {
	d := setupDaemon(t)
	d.Store.db.Exec(`INSERT INTO projects(id, name, path) VALUES(1, 'child', '/tmp/child')`)
	// Thread 500: root + working session; thread 600: root + done reply.
	d.Store.db.Exec(`INSERT INTO messages(id, channel_id, thread_id, from_project, to_project, content, status, ts)
		VALUES(500, 1, NULL, 'parent', 'child', 'do X please', 'message', datetime('now','-1 hour')),
		       (501, 1, 500, 'child', 'parent', 'working on it', 'ack', datetime('now')),
		       (600, 1, NULL, 'parent', 'child', 'do Y', 'message', datetime('now','-2 hours')),
		       (601, 1, 600, 'child', 'parent', 'did Y', 'done', datetime('now','-1 minute'))`)
	d.Store.db.Exec(`INSERT INTO sessions(project_id, agent_binary, model, status, pid, created_at, task_msg_id, root_thread_id)
		VALUES(1, 'opencode', 'default', 'active', 0, datetime('now'), 500, 500)`)

	resp := d.Handle(context.Background(), protocol.Request{Method: "thread_list", ID: 1})
	if resp.Error != nil {
		t.Fatalf("thread_list: %s", resp.Error.Message)
	}
	var items []ThreadListItem
	if err := json.Unmarshal(resp.Result, &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("want 2 threads, got %d: %+v", len(items), items)
	}
	// Newest activity first: 500 above 600.
	if items[0].ID != 500 || items[0].Status != "working" || items[0].MsgCount != 2 {
		t.Fatalf("thread 500 wrong: %+v", items[0])
	}
	if items[0].Title != "do X please" {
		t.Fatalf("title = %q", items[0].Title)
	}
	if items[1].ID != 600 || items[1].Status != "done" {
		t.Fatalf("thread 600 wrong: %+v", items[1])
	}
}

func TestSessionListProgressDetail(t *testing.T) {
	d := setupDaemon(t)
	d.Store.db.Exec(`INSERT INTO projects(id, name, path) VALUES(1, 'child', '/tmp/child')`)
	// Thread 500: root + progress note + todos; thread 600: bare root.
	d.Store.db.Exec(`INSERT INTO messages(id, channel_id, thread_id, from_project, to_project, content, status, ts)
		VALUES(500, 1, NULL, 'parent', 'child', 'do X', 'message', datetime('now','-1 hour')),
		       (501, 1, 500, 'child', 'parent', '80% done (eta ~5min)', 'progress', datetime('now','-2 minutes')),
		       (600, 1, NULL, 'parent', 'child', 'do Y', 'message', datetime('now','-2 hours'))`)
	d.Store.db.Exec(`INSERT INTO sessions(project_id, agent_binary, model, status, pid, created_at, task_msg_id, root_thread_id)
		VALUES(1, 'opencode', 'default', 'active', 0, datetime('now'), 500, 500),
		      (1, 'opencode', 'default', 'active', 0, datetime('now'), 600, 600)`)
	d.Store.db.Exec(`INSERT INTO todos(thread_id, content, state, updated_at)
		VALUES(500, 'run migration', 'pending', datetime('now')),
		      (500, 'codegen', 'done', datetime('now'))`)

	resp := d.Handle(context.Background(), protocol.Request{Method: "session_list", ID: 1})
	if resp.Error != nil {
		t.Fatalf("session_list: %s", resp.Error.Message)
	}
	var items []SessionListItem
	if err := json.Unmarshal(resp.Result, &items); err != nil {
		t.Fatal(err)
	}
	byThread := map[int64]SessionListItem{}
	for _, it := range items {
		byThread[it.ParentID] = it
	}
	p := byThread[500]
	if p.ProgressNote != "80% done (eta ~5min)" || p.ETAMinutes != 5 || p.ProgressAgeSecs <= 0 {
		t.Fatalf("thread 500 progress wrong: %+v", p)
	}
	if p.CurrentStep != "run migration" || p.TodosDone != 1 || p.TodosTotal != 2 {
		t.Fatalf("thread 500 todos wrong: %+v", p)
	}
	q := byThread[600]
	if q.ProgressNote != "" || q.ProgressAgeSecs != 0 || q.ETAMinutes != 0 ||
		q.CurrentStep != "" || q.TodosDone != 0 || q.TodosTotal != 0 {
		t.Fatalf("thread 600 must be zero-valued: %+v", q)
	}
}
