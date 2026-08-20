package daemon

import (
	"context"
	"os/exec"
	"strings"
)

// ============================================
// Opencode runtime — resume args + session capture
// ============================================
// ponytail: only opencode supported. Add runtime_<name>.go when codex/claude-code land.

// isOpencode reports whether binary is the opencode runtime.
func isOpencode(binary string) bool {
	return strings.Contains(binary, "opencode")
}

// opencodeResumeArgs returns args for `opencode run -s <session_id> [--title <name>] [-m <model>] <task>`.
func opencodeResumeArgs(sessionID, task, model, title string) []string {
	args := []string{"run", "-s", sessionID}
	if title != "" {
		args = append(args, "--title", title)
	}
	if model != "" && model != "default" {
		args = append(args, "-m", model)
	}
	return append(args, task)
}

// opencodeFreshArgs returns args for `opencode run [--title <name>] [-m <model>] <task>`.
func opencodeFreshArgs(task, model, title string) []string {
	args := []string{"run"}
	if title != "" {
		args = append(args, "--title", title)
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
