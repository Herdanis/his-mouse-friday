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

	// payment-agent engages user-service-agent.
	engageResp := mustSend(t, d, "engage_project_agent", map[string]any{
		"project": "companyA/user-service",
		"from":    "companyA/payment-service",
		"task":    "add payment_status field to User",
	})
	var engage daemon.EngageResult
	json.Unmarshal(engageResp.Result, &engage)
	if engage.SessionID == 0 || engage.ChannelID == 0 {
		t.Fatalf("engage returned %+v", engage)
	}

	// user-service-agent posts done.
	mustSend(t, d, "post_message", map[string]any{
		"channel": engage.ChannelID,
		"from":    "companyA/user-service",
		"to":      "companyA/payment-service",
		"content": "done, added payment_status to User model",
	})

	// payment-agent reads channel.
	readResp := mustSend(t, d, "read_channel", map[string]any{"channel": engage.ChannelID})
	var msgs []daemon.Message
	json.Unmarshal(readResp.Result, &msgs)
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}

	// Verify task message: from payment → user-service.
	task := msgs[0]
	if task.FromProject != "companyA/payment-service" || task.ToProject != "companyA/user-service" {
		t.Errorf("task sender: from=%q to=%q, want payment→user-service", task.FromProject, task.ToProject)
	}
	if task.Content != "add payment_status field to User" {
		t.Errorf("task content: %q", task.Content)
	}

	// Verify reply: from user-service → payment.
	reply := msgs[1]
	if reply.FromProject != "companyA/user-service" || reply.ToProject != "companyA/payment-service" {
		t.Errorf("reply sender: from=%q to=%q, want user-service→payment", reply.FromProject, reply.ToProject)
	}
	if reply.Content != "done, added payment_status to User model" {
		t.Errorf("reply content: %q", reply.Content)
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

	engageResp := send("engage_project_agent", map[string]any{
		"project": "companyA/user-service",
		"from":    "companyA/payment-service",
		"task":    "add payment_status field",
	})
	var engage daemon.EngageResult
	json.Unmarshal(engageResp.Result, &engage)
	if engage.ChannelID == 0 {
		t.Fatal("no channel id")
	}

	send("post_message", map[string]any{
		"channel": engage.ChannelID,
		"from":    "companyA/user-service",
		"to":      "companyA/payment-service",
		"content": "done",
	})
	readResp := send("read_channel", map[string]any{"channel": engage.ChannelID})
	var msgs []daemon.Message
	json.Unmarshal(readResp.Result, &msgs)
	if len(msgs) != 2 {
		t.Fatalf("got %d messages want 2", len(msgs))
	}
	// Verify sender identity on both messages.
	if msgs[0].FromProject != "companyA/payment-service" {
		t.Errorf("task from: %q want payment-service", msgs[0].FromProject)
	}
	if msgs[1].FromProject != "companyA/user-service" {
		t.Errorf("reply from: %q want user-service", msgs[1].FromProject)
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
