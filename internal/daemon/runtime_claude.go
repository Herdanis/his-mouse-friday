package daemon

import "strings"

// ============================================
// Claude Code runtime — headless args
// ============================================

func isClaude(binary string) bool {
	return strings.Contains(binary, "claude")
}

// claudeResumeArgs is unreachable via wakeAgent today — captureAgentSessionID
// only captures opencode sessions, so a claude spawn never gets an
// AgentSessionID to resume. Kept for when claude session capture lands.
func claudeResumeArgs(sessionID, task, model string) []string {
	args := []string{"-p", "--resume", sessionID}
	if model != "" && model != "default" {
		args = append(args, "--model", model)
	}
	return append(args, task)
}

func claudeFreshArgs(task, model string) []string {
	args := []string{"-p"}
	if model != "" && model != "default" {
		args = append(args, "--model", model)
	}
	return append(args, task)
}
