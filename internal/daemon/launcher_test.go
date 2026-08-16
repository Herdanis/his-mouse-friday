package daemon

import (
	"context"
	"os"
	"testing"
)

func TestLauncher_SpawnFakeAgent(t *testing.T) {
	dir := t.TempDir()
	// /bin/echo stands in for opencode; just needs to be an executable.
	l := &Launcher{Binary: "/bin/echo"}
	pid, err := l.Spawn(context.Background(), dir, "/bin/echo", "default", "runbook context")
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if pid <= 0 {
		t.Fatalf("invalid pid %d", pid)
	}
	proc, err := os.FindProcess(pid)
	if err == nil {
		_ = proc.Kill()
	}
}

func TestLauncher_SpawnMissingBinary(t *testing.T) {
	l := &Launcher{}
	_, err := l.Spawn(context.Background(), t.TempDir(), "/nonexistent/binary", "default", "")
	if err == nil {
		t.Fatal("expected error for missing binary")
	}
}
