package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"

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
	AgentName      string // opencode --agent; empty = hmf's default worker
	Runbook        string // MOUSE.md content
	Task           string // the task to perform
	FromID         string // engaging agent's "workspace/project" identity
	ProjectID      string // this project's "workspace/project" identity
	ChannelID      int64  // hmf channel for this conversation
	SessionID      int64  // hmf session id
	TaskMsgID      int64  // root thread id the agent must reply on
	OnExit         func(exitCode int)
	AgentSessionID string // non-empty = resume this agent session (opencode run -s)
	SessionName    string // hmf session name (<prefix>-<project>) — passed as --title to opencode run for later lookup
}

// Spawn starts the agent binary in dir with the task as initial prompt.
// Channel/session IDs + runbook passed via env vars so the spawned agent
// can read_channel, post_message back via hmf-mcp.
// buildArgs picks binary + args for a spawn. Pure function for testability.
// ponytail: runtimes live in runtime_<name>.go (opencode, claude).
func buildArgs(cfg SpawnConfig) (bin string, args []string) {
	bin = cfg.Binary
	task := cfg.Task
	args = []string{task}
	switch {
	case isOpencode(bin):
		if cfg.AgentSessionID != "" {
			args = opencodeResumeArgs(cfg.AgentSessionID, task, cfg.Model, cfg.SessionName, cfg.AgentName)
		} else {
			args = opencodeFreshArgs(task, cfg.Model, cfg.SessionName, cfg.AgentName)
		}
	case isClaude(bin):
		if cfg.AgentSessionID != "" {
			args = claudeResumeArgs(cfg.AgentSessionID, task, cfg.Model)
		} else {
			args = claudeFreshArgs(task, cfg.Model)
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
	// Scope: agent edits only its registered dir (belt-and-suspenders with the plugin).
	scope := "\n\n[SCOPE] Only edit files under " + cfg.Dir + ". A target outside it (another clone, a deployed/stow copy, home dir, absolute path elsewhere) → do NOT edit it; reply BLOCKED with the path you were asked to touch."
	// Reply rule: no TTY (can't answer bash:ask); no reply = parent polls forever.
	replyProtocol := "\n\n[REPLY RULE] Before exiting, post_message with thread_id=" + fmt.Sprintf("%d", cfg.TaskMsgID) + ", status=\"done\", and a one-line summary. Blocked (permission denied, file missing, ask-approval you can't get) → start the summary with \"BLOCKED: \" and the reason. Shell access → `hmf done \"<summary>\"` also works. No reply = parent waits forever."
	fullTask := cfg.Task + scope + replyProtocol
	cfg.Task = fullTask // so buildArgs emits the full task string
	bin, args := buildArgs(cfg)
	if l.Binary != "" {
		bin = l.Binary
	}
	if _, err := exec.LookPath(bin); err != nil {
		return 0, fmt.Errorf("agent binary %q not found: %w", bin, err)
	}
	// Watchdog: kills hung resumed sessions (opencode run -s doesn't exit) so
	// OnExit fires and the row isn't stuck "active" forever. 60min default so
	// legit long tasks survive; HMF_WATCHDOG overrides (Go duration, "0" = off).
	watchdog := 60 * time.Minute
	if v := os.Getenv("HMF_WATCHDOG"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			watchdog = d
		}
	}
	var spawnCtx context.Context
	var cancel context.CancelFunc
	if watchdog <= 0 {
		spawnCtx, cancel = context.WithCancel(ctx)
	} else {
		spawnCtx, cancel = context.WithTimeout(ctx, watchdog)
	}
	cmd := exec.CommandContext(spawnCtx, bin, args...)
	cmd.Dir = cfg.Dir
	// Otherwise discarded — only place a runtime error would surface.
	out := prefixWriter{prefix: fmt.Sprintf("agent#%d %s", cfg.SessionID, cfg.SessionName)}
	cmd.Stdout, cmd.Stderr = out, out
	logf("spawn", "session %d: exec %s %q dir=%s watchdog=%s", cfg.SessionID, bin, args, cfg.Dir, watchdog)
	// opencode reads $PWD (not getcwd) for project scoping — sync to cfg.Dir
	// or the session lands in the daemon's cwd's project, invisible to capture.
	cmd.Env = append(os.Environ(),
		"PWD="+cfg.Dir,
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
		cancel()
		return 0, fmt.Errorf("spawn %q: %w", bin, err)
	}
	go func() {
		_ = cmd.Wait()
		cancel()
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
// just-spawned process. Finds the session by its unique title (the hmf session
// name = <prefix>-<project>) so it doesn't race with the user's TUI session.
func captureAgentSessionID(cfg SpawnConfig) (string, error) {
	if !isOpencode(cfg.Binary) {
		return "", nil
	}
	out, err := opencodeListSessions(context.Background(), cfg.Binary, cfg.Dir)
	if err != nil {
		return "", fmt.Errorf("opencode session list: %w", err)
	}
	return opencodeFindSessionByTitle(out, cfg.SessionName), nil
}
