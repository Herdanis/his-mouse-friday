package tui

import (
	"encoding/json"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/herdanis/his-mouse-friday/internal/daemon"
)

// todo_threads returns an anonymous row struct in daemon.go — mirrored here.
func todoThreadsFixture() []todoThreadRow {
	return []todoThreadRow{{ThreadID: 500, Preview: "migrate auth", Done: 1, Total: 3}}
}

func TestTodosRenderThreadsAndOpen(t *testing.T) {
	tm := newTodosModel(recordingFetcher(new([]recordedCall), map[string]json.RawMessage{
		"todo_threads": mustJSON(todoThreadsFixture()),
		"todo_list": mustJSON([]daemon.Todo{
			{ID: 1, ThreadID: 500, Content: "write tests", State: "pending"},
			{ID: 2, ThreadID: 500, Content: "migrate auth", State: "done"},
		}),
	}))
	tm.refreshNow()
	v := tm.view(80, 24)
	if !strings.Contains(v, "migrate auth") {
		t.Fatalf("thread preview must render:\n%s", v)
	}
	tm2, _ := tm.update(tea.KeyMsg{Type: tea.KeyEnter})
	if tm2.openThread != 500 {
		t.Fatal("enter must open the selected thread")
	}
	v = tm2.view(80, 24)
	if !strings.Contains(v, "○ write tests") || !strings.Contains(v, "✓ migrate auth") {
		t.Fatalf("todo rows must show state glyph + content:\n%s", v)
	}
}

func TestTodosCycleState(t *testing.T) {
	var calls []recordedCall
	tm := newTodosModel(recordingFetcher(&calls, map[string]json.RawMessage{
		"todo_threads": mustJSON(todoThreadsFixture()),
		"todo_list":    mustJSON([]daemon.Todo{{ID: 1, ThreadID: 500, Content: "write tests", State: "pending"}}),
	}))
	tm.refreshNow()
	tm, _ = tm.update(tea.KeyMsg{Type: tea.KeyEnter})
	tm, _ = tm.update(tea.KeyMsg{Type: tea.KeySpace})
	c, ok := lastCall(calls, "todo_update")
	if !ok {
		t.Fatalf("space must call todo_update, got %+v", calls)
	}
	var p map[string]any
	if err := json.Unmarshal(c.Params, &p); err != nil {
		t.Fatalf("bad todo_update params: %v", err)
	}
	if p["id"] != float64(1) || p["state"] != "done" {
		t.Fatalf("space must cycle pending→done, got %+v", p)
	}
	tm2, _ := tm.update(tea.KeyMsg{Type: tea.KeyEsc})
	if tm2.openThread != 0 {
		t.Fatal("esc must return to thread list")
	}
}

func TestTodosAddFlow(t *testing.T) {
	var calls []recordedCall
	tm := newTodosModel(recordingFetcher(&calls, map[string]json.RawMessage{
		"todo_threads": mustJSON(todoThreadsFixture()),
		"todo_list":    mustJSON([]daemon.Todo{}),
	}))
	tm.refreshNow()
	tm, _ = tm.update(tea.KeyMsg{Type: tea.KeyEnter})
	tm, _ = tm.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	if !tm.adding {
		t.Fatal("a must open the add form")
	}
	tm, _ = tm.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("ship it")})
	tm, _ = tm.update(tea.KeyMsg{Type: tea.KeyEnter})
	if tm.adding {
		t.Fatal("enter must submit and close the form")
	}
	c, ok := lastCall(calls, "todo_add")
	if !ok {
		t.Fatalf("submit must call todo_add, got %+v", calls)
	}
	var p map[string]any
	if err := json.Unmarshal(c.Params, &p); err != nil {
		t.Fatalf("bad todo_add params: %v", err)
	}
	if p["thread_id"] != float64(500) || p["content"] != "ship it" {
		t.Fatalf("todo_add params wrong: %+v", p)
	}
}

func TestTodosDeleteConfirm(t *testing.T) {
	var calls []recordedCall
	tm := newTodosModel(recordingFetcher(&calls, map[string]json.RawMessage{
		"todo_threads": mustJSON(todoThreadsFixture()),
		"todo_list":    mustJSON([]daemon.Todo{{ID: 7, ThreadID: 500, Content: "x", State: "pending"}}),
	}))
	tm.refreshNow()
	tm, _ = tm.update(tea.KeyMsg{Type: tea.KeyEnter})
	tm, _ = tm.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	if !tm.confirm {
		t.Fatal("x must arm confirm")
	}
	tm, _ = tm.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	if tm.confirm {
		t.Fatal("n must cancel without deleting")
	}
	if _, ok := lastCall(calls, "todo_delete"); ok {
		t.Fatal("n must not delete")
	}
	tm, _ = tm.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	tm, _ = tm.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	c, ok := lastCall(calls, "todo_delete")
	if !ok {
		t.Fatalf("y must call todo_delete, got %+v", calls)
	}
	var p map[string]any
	if err := json.Unmarshal(c.Params, &p); err != nil {
		t.Fatalf("bad todo_delete params: %v", err)
	}
	if p["id"] != float64(7) {
		t.Fatalf("todo_delete params wrong: %+v", p)
	}
}

func TestTodosEmptyGuards(t *testing.T) {
	var calls []recordedCall
	tm := newTodosModel(recordingFetcher(&calls, map[string]json.RawMessage{}))
	tm.refreshNow()
	tm, _ = tm.update(tea.KeyMsg{Type: tea.KeyEnter}) // no threads
	tm, _ = tm.update(tea.KeyMsg{Type: tea.KeySpace}) // no todos
	tm, _ = tm.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	tm, _ = tm.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	tm, _ = tm.update(tea.KeyMsg{Type: tea.KeyDown})
	tm.view(80, 24)
	for _, c := range calls {
		if c.Method != "todo_threads" {
			t.Fatalf("no RPC may fire on empty state, got %+v", calls)
		}
	}
}

func TestTodosRerrClearsOnRefresh(t *testing.T) {
	tm := newTodosModel(recordingFetcher(new([]recordedCall), map[string]json.RawMessage{
		"todo_threads": mustJSON(todoThreadsFixture()),
	}))
	tm.refreshNow()
	tm.rerr = "boom"
	tm2, _ := tm.update(todosMsg{threads: todoThreadsFixture()})
	if tm2.rerr != "" {
		t.Fatalf("successful todosMsg must clear rerr, got %q", tm2.rerr)
	}
}
