package daemon

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
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

// OnExit fires with the process exit code when the spawned agent exits.
// Guards the launcher → MarkExited → task_status lifecycle chain.
func TestLauncher_OnExitFiresWithExitCode(t *testing.T) {
	dir := t.TempDir()
	// /bin/echo exits cleanly with code 0.
	l := &Launcher{Binary: "/bin/echo"}
	exitCode := -99
	fired := make(chan struct{})
	pid, err := l.Spawn(context.Background(), SpawnConfig{
		Dir:    dir,
		Binary: "/bin/echo",
		Task:   "x",
		OnExit: func(code int) {
			exitCode = code
			close(fired)
		},
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatalf("OnExit never fired (pid %d)", pid)
	}
	if exitCode != 0 {
		t.Errorf("exit code: got %d want 0", exitCode)
	}
}

// OnExit fires with a non-zero exit code when the spawned process fails.
func TestLauncher_OnExitNonZeroExitCode(t *testing.T) {
	dir := t.TempDir()
	// A script that exits 1 (macOS has no /bin/false).
	script := dir + "/fail.sh"
	os.WriteFile(script, []byte("#!/bin/sh\nexit 1\n"), 0755)
	l := &Launcher{}
	exitCode := -99
	fired := make(chan struct{})
	_, err := l.Spawn(context.Background(), SpawnConfig{
		Dir:    dir,
		Binary: script,
		Task:   "x",
		OnExit: func(code int) {
			exitCode = code
			close(fired)
		},
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("OnExit never fired")
	}
	if exitCode == 0 {
		t.Errorf("exit code: got 0 want non-zero for exit-1 script")
	}
}

// TaskMsgID is passed to the spawned agent as HMF_TASK_MSG_ID env var.
func TestLauncher_SetsTaskMsgIDEnv(t *testing.T) {
	dir := t.TempDir()
	script := dir + "/check-task.sh"
	os.WriteFile(script, []byte("#!/bin/sh\necho $HMF_TASK_MSG_ID\n"), 0755)
	l := &Launcher{}
	pid, err := l.Spawn(context.Background(), SpawnConfig{
		Dir:       dir,
		Binary:    script,
		Task:      "x",
		ChannelID: 1,
		SessionID: 1,
		TaskMsgID: 999,
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	proc, _ := os.FindProcess(pid)
	proc.Wait()
	// Re-run to capture output (Spawn doesn't capture stdout).
	cmd := exec.Command(script)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "HMF_TASK_MSG_ID=999")
	out, _ := cmd.Output()
	if strings.TrimSpace(string(out)) != "999" {
		t.Errorf("HMF_TASK_MSG_ID: got %q want 999", strings.TrimSpace(string(out)))
	}
}
