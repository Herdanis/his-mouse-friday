package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

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
