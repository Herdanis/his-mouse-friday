package daemon

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/herdanis/his-mouse-friday/internal/config"
	"github.com/herdanis/his-mouse-friday/internal/protocol"
)

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
