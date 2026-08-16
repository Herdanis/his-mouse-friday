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
