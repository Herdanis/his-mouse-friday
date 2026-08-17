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
	TaskMsgID int64  // hmf task message id; spawned agent threads its done reply to this
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
	// Project-scope confinement: pin the agent to its registered project dir so it
	// can't wander to a different clone / deployed copy / home directory. Handles
	// the case where a project has clones elsewhere (e.g. a stow-deployed copy in
	// the home dir) — the agent must edit the registered repo, not a sibling clone.
	// Belt-and-suspenders with the opencode plugin's hard edit guard.
	scope := "\n\n[PROJECT SCOPE] You are operating in the project directory: " + cfg.Dir + ". Edit ONLY files under this directory. If a requested target is outside " + cfg.Dir + " (a different clone, a deployed/stow copy, the home directory, an absolute path elsewhere), do NOT edit it — post a done reply stating the target is outside your project scope and giving the path you were asked to touch. Never edit files outside " + cfg.Dir + "."
	// Append reply protocol so spawned agent knows how to post done back, threaded
	// to the task message so wait_for_done matches THIS engage's reply (not a
	// sibling's) on the shared DM channel. Offer both paths: the post_message
	// MCP tool (needs no shell permission, works under bash:ask) and the one-line
	// `hmf done` CLI. Ban python/heredocs — the bash tool wrapper mangles them.
	replyProtocol := "\n\n[REPLY PROTOCOL] When your task is complete, post a done reply threaded to your task (thread_id=" + fmt.Sprintf("%d", cfg.TaskMsgID) + "). Do ONE of:\n  1. Call the post_message MCP tool with status=\"done\", thread_id=" + fmt.Sprintf("%d", cfg.TaskMsgID) + ", and a one-line summary. (Preferred — needs no shell permission.)\n  2. Or, if you have shell access, run: hmf done \"<one-line summary>\"\nDo NOT write python or heredocs to hit the daemon socket directly — the bash wrapper mangles multi-line commands."
	fullTask := cfg.Task + scope + replyProtocol
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
		"HMF_TASK_MSG_ID="+fmt.Sprintf("%d", cfg.TaskMsgID),
		"HMF_SOCK="+protocol.SocketPath(),
	)
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("spawn %q: %w", bin, err)
	}
	go func() { _ = cmd.Wait() }()
	return cmd.Process.Pid, nil
}
