package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"

	"github.com/herdanis/his-mouse-friday/internal/protocol"
)

type Launcher struct {
	Binary string // override for testing; empty = use binary arg
}

// SpawnConfig holds everything the spawned agent needs to do work + reply.
type SpawnConfig struct {
	Dir       string // repo path to run in
	Binary    string // agent binary (opencode, /bin/echo, etc.)
	Model     string // model id
	Runbook   string // MOUSE.md content
	Task      string // the task to perform
	FromID    string // engaging agent's "workspace/project" identity
	ProjectID string // this project's "workspace/project" identity
	ChannelID int64  // hmf channel for this conversation
	SessionID int64  // hmf session id
}

// Spawn starts the agent binary in dir with the task as initial prompt.
// Channel/session IDs + runbook passed via env vars so the spawned agent
// can read_channel, post_message back via hmf-mcp.
func (l *Launcher) Spawn(ctx context.Context, cfg SpawnConfig) (int, error) {
	bin := cfg.Binary
	if l.Binary != "" {
		bin = l.Binary
	}
	if _, err := exec.LookPath(bin); err != nil {
		return 0, fmt.Errorf("agent binary %q not found: %w", bin, err)
	}
	cmd := exec.CommandContext(ctx, bin, cfg.Task)
	cmd.Dir = cfg.Dir
	cmd.Env = append(os.Environ(),
		"HMF_RUNBOOK="+cfg.Runbook,
		"HMF_TASK="+cfg.Task,
		"HMF_FROM="+cfg.FromID,
		"HMF_PROJECT="+cfg.ProjectID,
		"HMF_CHANNEL_ID="+fmt.Sprintf("%d", cfg.ChannelID),
		"HMF_SESSION_ID="+fmt.Sprintf("%d", cfg.SessionID),
		"HMF_SOCK="+protocol.SocketPath(),
	)
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("spawn %q: %w", bin, err)
	}
	go func() { _ = cmd.Wait() }()
	return cmd.Process.Pid, nil
}
