package e2e

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/herdanis/his-mouse-friday/internal/config"
	"github.com/herdanis/his-mouse-friday/internal/daemon"
	"github.com/herdanis/his-mouse-friday/internal/protocol"
)

func TestVerticalSlice(t *testing.T) {
	// Set up two fake project dirs.
	wsDir := t.TempDir()
	paymentDir := filepath.Join(wsDir, "payment")
	userDir := filepath.Join(wsDir, "user-service")
	os.MkdirAll(paymentDir, 0755)
	os.MkdirAll(userDir, 0755)

	// user-service allows inbound.
	os.WriteFile(filepath.Join(userDir, "mouse.yaml"), []byte("agent:\n  primary:\n    provider: /bin/echo\na2a:\n  allow_inbound: true\n"), 0644)
	os.WriteFile(filepath.Join(userDir, "MOUSE.md"), []byte("# user-service runbook\nowns user model\n"), 0644)

	// Daemon with in-memory-ish store (temp db).
	store, _ := daemon.OpenStore(filepath.Join(t.TempDir(), "e2e.db"))
	defer store.Close()
	d := &daemon.Daemon{
		Store:       store,
		Registry:    &daemon.Registry{Store: store},
		Sessions:    &daemon.SessionStore{Store: store},
		Comms:       &daemon.Comms{Store: store},
		Launcher:    &daemon.Launcher{Binary: "/bin/echo"},
		MouseLoader: config.LoadMouse,
	}
	// Register workspace + projects via daemon methods.
	mustSend(t, d, "workspace_add", map[string]any{"name": "companyA"})
	mustSend(t, d, "project_add", map[string]any{"workspace": "companyA", "name": "payment-service", "path": paymentDir})
	mustSend(t, d, "project_add", map[string]any{"workspace": "companyA", "name": "user-service", "path": userDir})

	// payment posts a task to general mentioning @companyA/user-service —
	// thread root. Daemon wakes user-service (spawns /bin/echo).
	postResp := mustSend(t, d, "post_message", map[string]any{
		"from":    "companyA/payment-service",
		"to":      "companyA/user-service",
		"content": "add payment_status field to User",
	})
	var pr daemon.PostResult
	json.Unmarshal(postResp.Result, &pr)
	if pr.MessageID == 0 {
		t.Fatalf("post returned no message id: %+v", postResp)
	}
	taskID := pr.MessageID

	// user-service posts in_progress + a threaded done reply (thread_id=taskID).
	mustSend(t, d, "post_message", map[string]any{
		"thread_id": taskID,
		"from":      "companyA/user-service",
		"to":        "companyA/payment-service",
		"content":   "working on it",
		"status":    "in_progress",
	})
	mustSend(t, d, "post_message", map[string]any{
		"thread_id": taskID,
		"from":      "companyA/user-service",
		"to":        "companyA/payment-service",
		"content":   "done, added payment_status to User model",
		"status":    "done",
	})

	// payment reads the thread — gets root + in_progress + done.
	threadResp := mustSend(t, d, "read_thread", map[string]any{"thread_id": taskID})
	var msgs []daemon.Message
	json.Unmarshal(threadResp.Result, &msgs)
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages in thread, got %d", len(msgs))
	}
	task := msgs[0]
	if task.FromProject != "companyA/payment-service" || task.ToProject != "companyA/user-service" {
		t.Errorf("task sender: from=%q to=%q, want payment→user-service", task.FromProject, task.ToProject)
	}
	if task.Content != "add payment_status field to User" {
		t.Errorf("task content: %q", task.Content)
	}
	reply := msgs[2]
	if reply.FromProject != "companyA/user-service" || reply.ToProject != "companyA/payment-service" {
		t.Errorf("reply sender: from=%q to=%q, want user-service→payment", reply.FromProject, reply.ToProject)
	}
	if reply.Status != "done" || reply.Content != "done, added payment_status to User model" {
		t.Errorf("reply: status=%q content=%q", reply.Status, reply.Content)
	}
}

func mustSend(t *testing.T, d *daemon.Daemon, method string, params any) protocol.Response {
	t.Helper()
	b, _ := json.Marshal(params)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp := d.Handle(ctx, protocol.Request{Method: method, Params: b, ID: 1})
	if resp.Error != nil {
		t.Fatalf("%s: %s", method, resp.Error.Message)
	}
	return resp
}

// TestVerticalSlice_OverSocket proves the slice over the real unix-socket
// transport (line-delimited JSON framing + serveConn loop), not just Handle.
// Guards the wire format the MCP shim and CLI actually use.
func TestVerticalSlice_OverSocket(t *testing.T) {
	wsDir := t.TempDir()
	userDir := filepath.Join(wsDir, "user-service")
	os.MkdirAll(userDir, 0755)
	os.WriteFile(filepath.Join(userDir, "mouse.yaml"), []byte("agent:\n  primary:\n    provider: /bin/echo\na2a:\n  allow_inbound: true\n"), 0644)
	os.WriteFile(filepath.Join(userDir, "MOUSE.md"), []byte("# user-service\n"), 0644)

	store, _ := daemon.OpenStore(filepath.Join(t.TempDir(), "sock-e2e.db"))
	defer store.Close()
	sock := filepath.Join(t.TempDir(), "daemon.sock")
	d := &daemon.Daemon{
		Store:       store,
		Registry:    &daemon.Registry{Store: store},
		Sessions:    &daemon.SessionStore{Store: store},
		Comms:       &daemon.Comms{Store: store},
		Launcher:    &daemon.Launcher{Binary: "/bin/echo"},
		MouseLoader: config.LoadMouse,
		Sock:        sock,
	}
	// shutdownCh is unexported; NewDaemon inits it but overrides Launcher.
	// Use NewDaemon then swap Launcher + Sock so Spawn uses /bin/echo.
	d2, err := daemon.NewDaemon(sock, filepath.Join(t.TempDir(), "sock-e2e2.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Store.Close()
	d2.Launcher = &daemon.Launcher{Binary: "/bin/echo"}
	d = d2

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveErr := make(chan error, 1)
	go func() { serveErr <- d.Serve(ctx) }()

	// Wait for socket to accept.
	var conn net.Conn
	deadline, cancelWait := context.WithTimeout(ctx, 2*time.Second)
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

	send := func(method string, params any) protocol.Response {
		t.Helper()
		c, derr := net.Dial("unix", sock)
		if derr != nil {
			t.Fatalf("dial: %v", derr)
		}
		defer c.Close()
		b, _ := json.Marshal(params)
		enc := json.NewEncoder(c)
		dec := json.NewDecoder(c)
		if err := enc.Encode(&protocol.Request{Method: method, Params: b, ID: 1}); err != nil {
			t.Fatalf("encode: %v", err)
		}
		var resp protocol.Response
		if err := dec.Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.Error != nil {
			t.Fatalf("%s: %s", method, resp.Error.Message)
		}
		return resp
	}

	send("workspace_add", map[string]any{"name": "companyA"})
	send("project_add", map[string]any{"workspace": "companyA", "name": "payment-service", "path": wsDir})
	send("project_add", map[string]any{"workspace": "companyA", "name": "user-service", "path": userDir})

	// Post a task to general — wakes user-service (/bin/echo), thread root.
	postResp := send("post_message", map[string]any{
		"from":    "companyA/payment-service",
		"to":      "companyA/user-service",
		"content": "add payment_status field",
	})
	var pr daemon.PostResult
	json.Unmarshal(postResp.Result, &pr)
	if pr.MessageID == 0 {
		t.Fatal("no message id")
	}

	// Reply in-thread.
	send("post_message", map[string]any{
		"thread_id": pr.MessageID,
		"from":      "companyA/user-service",
		"to":        "companyA/payment-service",
		"content":   "done",
		"status":    "done",
	})

	// Read thread — root + done.
	threadResp := send("read_thread", map[string]any{"thread_id": pr.MessageID})
	var msgs []daemon.Message
	json.Unmarshal(threadResp.Result, &msgs)
	if len(msgs) != 2 {
		t.Fatalf("got %d messages want 2", len(msgs))
	}
	if msgs[0].FromProject != "companyA/payment-service" {
		t.Errorf("task from: %q want payment-service", msgs[0].FromProject)
	}
	if msgs[1].FromProject != "companyA/user-service" || msgs[1].Status != "done" {
		t.Errorf("reply: from=%q status=%q want user-service/done", msgs[1].FromProject, msgs[1].Status)
	}
	conn.Close()

	// Shutdown cleanly.
	send("shutdown", struct{}{})
	select {
	case err := <-serveErr:
		if err != nil {
			t.Errorf("Serve returned %v want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after shutdown")
	}
}
