package cli

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
)

// captureStdout redirects os.Stdout for fn and returns what it printed.
// cli.go commands write via fmt.Println/fmt.Fprintf(os.Stdout, ...) directly
// (not cmd.OutOrStdout()), so redirecting the real os.Stdout is the only way
// to capture output without touching production code.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	defer func() { os.Stdout = old }()

	fn()

	w.Close()
	var buf bytes.Buffer
	io.Copy(&buf, r)
	return buf.String()
}

// hmf task list (no thread arg) shows every thread that has todos, with a
// DONE/TOTAL count and a content preview.
func TestCLI_TaskList_Overview(t *testing.T) {
	threadParentID, _, cleanup := spinCLIDaemon(t)
	defer cleanup()

	mustCLICall(t, "todo_add", map[string]any{"thread_id": threadParentID, "content": "step one"})
	mustCLICall(t, "todo_add", map[string]any{"thread_id": threadParentID, "content": "step two"})

	out := captureStdout(t, func() {
		root := NewRootCmd()
		root.SetArgs([]string{"task", "list"})
		if err := root.Execute(); err != nil {
			t.Fatalf("task list: %v", err)
		}
	})

	if !strings.Contains(out, "0/2") {
		t.Errorf("expected 0/2 done/total, got: %s", out)
	}
	if !strings.Contains(out, itoa(threadParentID)) {
		t.Errorf("expected thread id %d in output, got: %s", threadParentID, out)
	}
}

// hmf task list <thread_id> shows the todos for that one thread.
func TestCLI_TaskList_OneThread(t *testing.T) {
	threadParentID, _, cleanup := spinCLIDaemon(t)
	defer cleanup()

	mustCLICall(t, "todo_add", map[string]any{"thread_id": threadParentID, "content": "add field to User model"})

	out := captureStdout(t, func() {
		root := NewRootCmd()
		root.SetArgs([]string{"task", "list", itoa(threadParentID)})
		if err := root.Execute(); err != nil {
			t.Fatalf("task list <id>: %v", err)
		}
	})

	if !strings.Contains(out, "add field to User model") {
		t.Errorf("expected todo content in output, got: %s", out)
	}
	if !strings.Contains(out, "pending") {
		t.Errorf("expected state 'pending' in output, got: %s", out)
	}
}

// hmf task list with no todos anywhere prints a friendly empty message.
func TestCLI_TaskList_Empty(t *testing.T) {
	_, _, cleanup := spinCLIDaemon(t)
	defer cleanup()

	out := captureStdout(t, func() {
		root := NewRootCmd()
		root.SetArgs([]string{"task", "list"})
		if err := root.Execute(); err != nil {
			t.Fatalf("task list: %v", err)
		}
	})

	if !strings.Contains(out, "no threads with todos") {
		t.Errorf("expected empty-state message, got: %s", out)
	}
}
