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
	// SpawnFn overrides Spawn for testing. If nil, real Spawn runs.
	SpawnFn func(cfg SpawnConfig) (int, error)
}

// SpawnConfig holds everything the spawned agent needs to do work + reply.
type SpawnConfig struct {
	Dir            string // repo path to run in
	Binary         string // agent binary (opencode, /bin/echo, etc.)
	Model          string // model id
	Runbook        string // MOUSE.md content
	Task           string // the task to perform
	FromID         string // engaging agent's "workspace/project" identity
	ProjectID      string // this project's "workspace/project" identity
	ChannelID      int64  // hmf channel for this conversation
	SessionID      int64  // hmf session id
	TaskMsgID      int64  // hmf task message id; spawned agent threads its done reply to this
	OnExit         func(exitCode int)
	AgentSessionID string // non-empty = resume this agent session via runtime-specific resume flag
}

// Spawn starts the agent binary in dir with the task as initial prompt.
// Channel/session IDs + runbook passed via env vars so the spawned agent
// can read_channel, post_message back via hmf-mcp.
// buildArgs decides the binary + args for an agent spawn. Pure function
// so tests can verify resume vs. fresh arg construction without spawning.
// ponytail: only opencode supported; switch on binary name when codex /
// claude-code land — add runtime_<name>.go with the same fn signatures.
func buildArgs(cfg SpawnConfig) (bin string, args []string) {
	bin = cfg.Binary
	task := cfg.Task
	args = []string{task}
	switch {
	case isOpencode(bin):
		if cfg.AgentSessionID != "" {
			args = opencodeResumeArgs(cfg.AgentSessionID, task, cfg.Model)
		} else {
			args = opencodeFreshArgs(task, cfg.Model)
		}
	default:
		// Unknown runtime: no resume support. Run <bin> <task> directly.
	}
	return bin, args
}

func (l *Launcher) Spawn(ctx context.Context, cfg SpawnConfig) (int, error) {
	if l.SpawnFn != nil {
		return l.SpawnFn(cfg)
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
	// MUST-REPLY rule: a spawned agent has no TTY, so it can't answer bash:ask
	// prompts. Without this rule, agents that hit a permission wall just exit
	// silently and the orchestrator polls forever. Forcing a done reply on every
	// exit path (success, blocked, or failed) lets the orchestrator stop polling
	// and surface the blocker to the user.
	replyProtocol := "\n\n[REPLY PROTOCOL] You MUST post a done reply before exiting — whether you completed the task, hit a blocker, or couldn't start. Thread it to your task (thread_id=" + fmt.Sprintf("%d", cfg.TaskMsgID) + "). Use the post_message MCP tool with status=\"done\" and a one-line summary (preferred — needs no shell permission, works under bash:ask). If blocked (permission denied, file not found, missing tool, command requires ask-approval you can't get), prefix the summary with \"BLOCKED: \" and state what stopped you. Alternatively, if you have shell access, run: hmf done \"<summary>\". Do NOT write python or heredocs to hit the daemon socket directly — the bash wrapper mangles multi-line commands. Never exit without posting a done reply."
	fullTask := cfg.Task + scope + replyProtocol
	cfg.Task = fullTask // so buildArgs emits the full task string
	bin, args := buildArgs(cfg)
	if l.Binary != "" {
		bin = l.Binary
	}
	if _, err := exec.LookPath(bin); err != nil {
		return 0, fmt.Errorf("agent binary %q not found: %w", bin, err)
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
	go func() {
		_ = cmd.Wait()
		if cfg.OnExit != nil {
			code := -1
			if cmd.ProcessState != nil {
				code = cmd.ProcessState.ExitCode()
			}
			cfg.OnExit(code)
		}
	}()
	return cmd.Process.Pid, nil
}

// ============================================
// captureAgentSessionID — post-spawn opencode session list query
// ============================================

// captureAgentSessionID queries the agent runtime for the session ID of the
// just-spawned process. Currently delegates to opencode's session list. When
// other runtimes land, switch on cfg.Binary and call <runtime>ListSessions +
// <runtime>ParseSessionID.
func captureAgentSessionID(cfg SpawnConfig) (string, error) {
	if !isOpencode(cfg.Binary) {
		return "", nil // unknown runtime: no capture, no resume
	}
	out, err := opencodeListSessions(context.Background(), cfg.Binary, cfg.Dir)
	if err != nil {
		return "", fmt.Errorf("opencode session list: %w", err)
	}
	return opencodeParseSessionID(out), nil
}
