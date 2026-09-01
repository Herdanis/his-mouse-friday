package daemon

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// ============================================
// Opencode runtime — resume args + session capture
// ============================================
// ponytail: only opencode supported. Add runtime_<name>.go when codex/claude-code land.

// isOpencode reports whether binary is the opencode runtime.
func isOpencode(binary string) bool {
	return strings.Contains(binary, "opencode")
}

// defaultAgentName is the agent hmf spawns with when mouse.yaml doesn't name
// one. Explicit so a child never silently inherits the user's own
// `default_agent` — that default may be a narrow one (e.g. a surgical
// 1-2 file editor with no shell) that refuses ordinary delegated work.
const defaultAgentName = "hmf-worker"

// opencodeResumeArgs returns args for `opencode run -s <id> [--agent <a>] [--title <n>] [-m <model>] <task>`.
func opencodeResumeArgs(sessionID, task, model, title, agent string) []string {
	return append(opencodeCommonArgs([]string{"run", "-s", sessionID}, model, title, agent), task)
}

// opencodeFreshArgs returns args for `opencode run [--agent <a>] [--title <n>] [-m <model>] <task>`.
func opencodeFreshArgs(task, model, title, agent string) []string {
	return append(opencodeCommonArgs([]string{"run"}, model, title, agent), task)
}

func opencodeCommonArgs(args []string, model, title, agent string) []string {
	if agent == "" {
		agent = defaultAgentName
	}
	// --auto: headless spawns have no TTY, so opencode's native `ask` prompts
	// hang forever. Auto-approves only what isn't explicitly denied, so deny
	// rules still hold — unlike a blanket `bash: "*": "allow"` in the config.
	args = append(args, "--auto", "--agent", agent)
	if title != "" {
		args = append(args, "--title", title)
	}
	if model != "" && model != "default" {
		args = append(args, "-m", model)
	}
	return args
}

// opencodeListSessions runs `<binary> session list` in dir, returns stdout.
func opencodeListSessions(ctx context.Context, binary, dir string) (string, error) {
	cmd := exec.CommandContext(ctx, binary, "session", "list")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// opencodeParseSessionID extracts the first `ses_` ID from session list output.
func opencodeParseSessionID(output string) string {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "ses_") {
			fields := strings.Fields(line)
			if len(fields) > 0 && fields[0] != "" {
				return fields[0]
			}
		}
	}
	return ""
}

// opencodeFindSessionByTitle returns the ses_ ID from the line containing
// title. Title is unique per hmf session (random prefix) — no TUI race.
func opencodeFindSessionByTitle(output, title string) string {
	if title == "" {
		return opencodeParseSessionID(output)
	}
	for _, line := range strings.Split(output, "\n") {
		if strings.Contains(line, title) {
			for _, field := range strings.Fields(line) {
				if strings.HasPrefix(field, "ses_") {
					return field
				}
			}
		}
	}
	return ""
}

// runtimeModelAvailable probes whether a runtime can run a model.
// checkable=false = no probe for this runtime (caller assumes available).
// ponytail: opencode probe only; add per-runtime cases in runtime_<name>.go.
func runtimeModelAvailable(binary, model string) (ok, checkable bool) {
	if !isOpencode(binary) {
		return true, false
	}
	// Bounded: this runs synchronously on the post_message request path
	// (resolveAgent), so a hung `<binary> models` must not stall the caller.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, binary, "models").Output()
	if err != nil {
		return true, false // probe failed — assume available rather than block
	}
	return strings.Contains(string(out), model), true
}
