package e2e

import (
	"context"
	"encoding/json"
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
	os.WriteFile(filepath.Join(userDir, "mouse.yaml"), []byte("agent:\n  primary: /bin/echo\na2a:\n  allow_inbound: true\n"), 0644)
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
		"content": "done, added payment_status to User model",
	})

	// payment-agent reads channel.
	readResp := mustSend(t, d, "read_channel", map[string]any{"channel": engage.ChannelID})
	var msgs []daemon.Message
	json.Unmarshal(readResp.Result, &msgs)
	if len(msgs) == 0 {
		t.Fatal("no messages in channel")
	}

	// Verify the task message + the response are both present.
	foundTask, foundDone := false, false
	for _, m := range msgs {
		if m.Content == "add payment_status field to User" {
			foundTask = true
		}
		if m.Content == "done, added payment_status to User model" {
			foundDone = true
		}
	}
	if !foundTask || !foundDone {
		t.Errorf("expected task+done messages, got %+v", msgs)
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
