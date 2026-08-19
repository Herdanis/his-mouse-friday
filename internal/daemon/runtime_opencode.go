package daemon

import (
	"context"
	"os/exec"
	"strings"
)

// ============================================
// Opencode runtime — resume args + session capture
// ============================================
// ponytail: only opencode supported. When codex / claude-code land, add
// runtime_codex.go (etc.) with the same function signatures and extend the
// switch in buildArgs + captureAgentSessionID to pick by binary name.

// isOpencode reports whether binary is the opencode runtime.
func isOpencode(binary string) bool {
	return strings.Contains(binary, "opencode")
}

// opencodeResumeArgs returns args for `opencode run --auto -s <session_id> [--agent <profile>] [-m <model>] <task>`.
// --auto: spawned agents have no TTY, can't answer bash:ask prompts — auto-approve
// non-denied permissions so the agent can actually work. Denies in opencode.json
// still block (only "ask" is auto-approved).
func opencodeResumeArgs(sessionID, task, model, profile string) []string {
	args := []string{"run", "--auto", "-s", sessionID}
	if profile != "" {
		args = append(args, "--agent", profile)
	}
	if model != "" && model != "default" {
		args = append(args, "-m", model)
	}
	return append(args, task)
}

// opencodeFreshArgs returns args for `opencode run --auto [--agent <profile>] [-m <model>] <task>`.
func opencodeFreshArgs(task, model, profile string) []string {
	args := []string{"run", "--auto"}
	if profile != "" {
		args = append(args, "--agent", profile)
	}
	if model != "" && model != "default" {
		args = append(args, "-m", model)
	}
	return append(args, task)
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
