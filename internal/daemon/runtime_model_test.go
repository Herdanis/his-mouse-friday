package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Spawns must not inject hmf's own agent: opencode's default agent runs
// unless mouse.yaml names one explicitly.
func TestOpencodeArgsNoDefaultAgent(t *testing.T) {
	args := opencodeFreshArgs("do X", "default", "", "")
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "--agent") {
		t.Fatalf("no agent name configured — args must not carry --agent, got: %v", args)
	}
	withName := opencodeFreshArgs("do X", "default", "", "reviewer")
	if !strings.Contains(strings.Join(withName, " "), "--agent reviewer") {
		t.Fatalf("configured agent must be passed through, got: %v", withName)
	}
}

func TestRuntimeModelAvailable(t *testing.T) {
	// Fake opencode binary whose `models` output lists exactly one model.
	dir := t.TempDir()
	bin := filepath.Join(dir, "opencode-fake")
	script := "#!/bin/sh\necho '  anthropic/claude-sonnet-5   5.0  '\necho '  other/model  1.0'\n"
	if err := os.WriteFile(bin, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	if ok, checkable := runtimeModelAvailable(bin, "anthropic/claude-sonnet-5"); !ok || !checkable {
		t.Errorf("listed model: got ok=%v checkable=%v, want true/true", ok, checkable)
	}
	if ok, checkable := runtimeModelAvailable(bin, "nope/missing"); ok || !checkable {
		t.Errorf("missing model: got ok=%v checkable=%v, want false/true", ok, checkable)
	}

	// Failing probe (binary exits nonzero) → assume available.
	if ok, checkable := runtimeModelAvailable(filepath.Join(dir, "nothere-opencode"), "m"); !ok || checkable {
		t.Errorf("failing probe: got ok=%v checkable=%v, want true/false", ok, checkable)
	}

	// Non-opencode runtime → no probe.
	if ok, checkable := runtimeModelAvailable("/bin/echo", "m"); !ok || checkable {
		t.Errorf("non-opencode: got ok=%v checkable=%v, want true/false", ok, checkable)
	}
}
