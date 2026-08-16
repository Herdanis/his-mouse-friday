package daemon

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestLauncher_SpawnFakeAgent(t *testing.T) {
	dir := t.TempDir()
	l := &Launcher{Binary: "/bin/echo"}
	pid, err := l.Spawn(context.Background(), SpawnConfig{
		Dir:       dir,
		Binary:    "/bin/echo",
		Model:     "default",
		Runbook:   "runbook context",
		Task:      "do something",
		FromID:    "ws/a",
		ProjectID: "ws/b",
		ChannelID: 1,
		SessionID: 1,
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if pid <= 0 {
		t.Fatalf("invalid pid %d", pid)
	}
	proc, _ := os.FindProcess(pid)
	_ = proc.Kill()
}

func TestLauncher_SpawnMissingBinary(t *testing.T) {
	l := &Launcher{}
	_, err := l.Spawn(context.Background(), SpawnConfig{
		Dir:    t.TempDir(),
		Binary: "/nonexistent/binary",
	})
	if err == nil {
		t.Fatal("expected error for missing binary")
	}
}

func TestLauncher_SpawnSetsEnvVars(t *testing.T) {
	dir := t.TempDir()
	script := dir + "/check-env.sh"
	os.WriteFile(script, []byte("#!/bin/sh\nenv | grep ^HMF_ | sort\n"), 0755)
	l := &Launcher{}
	pid, err := l.Spawn(context.Background(), SpawnConfig{
		Dir:       dir,
		Binary:    script,
		Task:      "test task",
		FromID:    "ws/a",
		ProjectID: "ws/b",
		ChannelID: 42,
		SessionID: 7,
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	proc, _ := os.FindProcess(pid)
	proc.Wait()
	// Re-run to capture output (Spawn is fire-and-forget, no stdout capture).
	cmd := exec.Command(script)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"HMF_TASK=test task",
		"HMF_FROM=ws/a",
		"HMF_PROJECT=ws/b",
		"HMF_CHANNEL_ID=42",
		"HMF_SESSION_ID=7",
	)
	out, _ := cmd.Output()
	got := string(out)
	for _, w := range []string{"HMF_CHANNEL_ID=42", "HMF_FROM=ws/a", "HMF_PROJECT=ws/b", "HMF_SESSION_ID=7", "HMF_TASK=test task"} {
		if !strings.Contains(got, w) {
			t.Errorf("missing env %q in:\n%s", w, got)
		}
	}
}
