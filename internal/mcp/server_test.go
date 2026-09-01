package mcp

import (
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

// Regression: two parallel delegations to different projects must NOT share a
// thread. They did — a single session-scoped currentThread bound the second
// post onto the first's thread, and the daemon's wake guard (first agent still
// active on that thread) then suppressed the second spawn entirely.
func TestResolveThreadID_ParallelDelegationsGetSeparateThreads(t *testing.T) {
	byRecipient := map[string]int64{}
	var currentThread int64

	// First delegation → new root.
	if got := resolveThreadID(0, "penny-pincher", byRecipient, currentThread); got != 0 {
		t.Fatalf("first delegation: got thread %d, want 0 (new root)", got)
	}
	byRecipient["penny-pincher"] = 97
	currentThread = 97

	// Second delegation, different project → must also be a new root.
	if got := resolveThreadID(0, "mouse-for-sale", byRecipient, currentThread); got != 0 {
		t.Fatalf("parallel delegation to another project: got thread %d, want 0 (new root); "+
			"binding it to %d would make the daemon suppress its wake", got, got)
	}

	// Follow-up to the SAME project → continues that project's thread (resume).
	if got := resolveThreadID(0, "penny-pincher", byRecipient, currentThread); got != 97 {
		t.Fatalf("follow-up to same project: got %d, want 97", got)
	}

	// Explicit thread_id always wins.
	if got := resolveThreadID(555, "mouse-for-sale", byRecipient, currentThread); got != 555 {
		t.Fatalf("explicit thread_id: got %d, want 555", got)
	}

	// A reply with no recipient still binds to the session's own thread.
	if got := resolveThreadID(0, "", byRecipient, currentThread); got != 97 {
		t.Fatalf("to-less reply: got %d, want 97", got)
	}
}
