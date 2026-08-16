package daemon

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
)

type Launcher struct {
	Binary string // override for testing; empty = use binary arg
}

// Spawn starts the agent binary in dir with runbookCtx as an argument.
// Returns the PID. MVP: fire-and-forget; we don't pipe stdin.
func (l *Launcher) Spawn(ctx context.Context, dir, binary, model, runbookCtx string) (int, error) {
	bin := binary
	if l.Binary != "" {
		bin = l.Binary
	}
	if _, err := exec.LookPath(bin); err != nil {
		return 0, fmt.Errorf("agent binary %q not found: %w", bin, err)
	}
	cmd := exec.CommandContext(ctx, bin, runbookCtx)
	cmd.Dir = dir
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("spawn %q: %w", bin, err)
	}
	// Fire-and-forget: agent runs independently. Reap on exit to avoid zombies.
	go func() { _ = cmd.Wait() }()
	return cmd.Process.Pid, nil
}

// pidToString formats a PID for status display.
func pidToString(pid int) string { return strconv.Itoa(pid) }
