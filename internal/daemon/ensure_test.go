package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/herdanis/his-mouse-friday/internal/protocol"
)

func TestEnsureRunningNotRunningFailsWithoutBinary(t *testing.T) {
	// HMF_STATE_DIR points somewhere with no daemon running; PATH stripped of
	// hmf so the LookPath fails deterministically.
	t.Setenv("HMF_STATE_DIR", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	err := EnsureRunning()
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("want 'not found' error, got %v", err)
	}
}

// spinEnsureDaemon brings up a real daemon listening on the test state dir's
// socket, so EnsureRunning's fast path (dial succeeds) is exercised.
func spinEnsureDaemon(t *testing.T) *Daemon {
	t.Helper()
	t.Setenv("HMF_STATE_DIR", t.TempDir())
	d, err := NewDaemon(protocol.SocketPath(), protocol.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go d.Serve(ctx)
	for deadline := time.Now().Add(awaitSockTimeout); time.Now().Before(deadline); {
		if _, err := protocol.Call("status", nil); err == nil {
			return d
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("daemon never came up")
	return nil
}

func TestEnsureRunningAlreadyUp(t *testing.T) {
	spinEnsureDaemon(t)
	if err := EnsureRunning(); err != nil {
		t.Fatalf("daemon already up: %v", err)
	}
}
