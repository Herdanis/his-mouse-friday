package mcp

import (
	"strings"
	"testing"
)

// TestNewServer_NoPanic guards the jsonschema-tag regression: AddTool panics
// at construction if any input type's jsonschema tag matches the forbidden
// ^[^ \t\n]*= regex (caught manually in Task 10; this test automates it).
// If newServer("") returns without panic, all 5 tools registered cleanly.
func TestNewServer_NoPanic(t *testing.T) {
	srv := newServer("")
	if srv == nil {
		t.Fatal("newServer returned nil")
	}
}

// Everything dispatched from one session joins that session's thread, so
// work split across two projects stays one parent task. This is only safe
// because the daemon's wake guard is per project — see
// TestHandle_SecondProjectOnSameThreadStillWakes.
func TestResolveThreadID_KeepsRelatedWorkOnOneParent(t *testing.T) {
	byRecipient := map[string]int64{}
	var currentThread int64

	// First delegation of the session opens the parent thread.
	if got := resolveThreadID(0, "penny-pincher", byRecipient, currentThread); got != 0 {
		t.Fatalf("first delegation: got thread %d, want 0 (new root)", got)
	}
	currentThread = 97

	// A second project on the same goal joins that parent, not a new root.
	if got := resolveThreadID(0, "mouse-for-sale", byRecipient, currentThread); got != 97 {
		t.Fatalf("second project: got %d, want 97 (same parent)", got)
	}

	// Explicit thread_id always wins.
	if got := resolveThreadID(555, "mouse-for-sale", byRecipient, currentThread); got != 555 {
		t.Fatalf("explicit thread_id: got %d, want 555", got)
	}

	// A reply with no recipient also stays on the session's thread.
	if got := resolveThreadID(0, "", byRecipient, currentThread); got != 97 {
		t.Fatalf("to-less reply: got %d, want 97", got)
	}
}

// An unregistered caller should still be attributable: name it by the
// directory it runs in, distinguishable from a real "workspace/project".
func TestDirIdentity(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/Users/me/Project/ledger", "dir:ledger"},
		{"/Users/me/Project/ledger/", "dir:ledger"},
		{"/", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := dirIdentity(c.in); got != c.want {
			t.Errorf("dirIdentity(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// Must never look like a project, or the daemon would try to resolve it
	// as a recipient and fail the post.
	if strings.Contains(dirIdentity("/Users/me/ledger"), "/") {
		t.Error("dir identity must not contain a slash")
	}
}
