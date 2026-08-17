package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

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
	// Append reply protocol so spawned agent knows to post done back.
	replyProtocol := "\n\n[REPLY PROTOCOL] When your task is complete, call the post_message MCP tool with status=\"done\" and a brief summary of what you did. This signals completion to the engaging agent."
	fullTask := cfg.Task + replyProtocol
	// opencode needs "run" subcommand for non-interactive mode.
	// Pass -m only when model is set and not "default" (let opencode use its global default).
	args := []string{fullTask}
	if strings.Contains(bin, "opencode") {
		args = []string{"run"}
		if cfg.Model != "" && cfg.Model != "default" {
			args = append(args, "-m", cfg.Model)
		}
		args = append(args, fullTask)
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = cfg.Dir
	cmd.Env = append(os.Environ(),
		"HMF_RUNBOOK="+cfg.Runbook,
		"HMF_TASK="+fullTask,
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
