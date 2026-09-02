package daemon

import (
	"path/filepath"
	"testing"
)

func TestTodoStore_RoundTrip(t *testing.T) {
	s, _ := OpenStore(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	ts := &TodoStore{Store: s}
	s.db.Exec(`INSERT INTO messages(id, channel_id, from_project, content, ts) VALUES(17, 1, 'a', 'task', datetime('now'))`)

	td, err := ts.Add(17, "add field to User model")
	if err != nil || td.ID == 0 || td.State != "pending" {
		t.Fatalf("add: %+v err=%v", td, err)
	}
	if _, err := ts.Add(17, "update schema"); err != nil {
		t.Fatalf("add2: %v", err)
	}
	list, err := ts.List(17)
	if err != nil || len(list) != 2 {
		t.Fatalf("list: %d items err=%v", len(list), err)
	}
	if err := ts.Update(list[0].ID, "done"); err != nil {
		t.Fatalf("update: %v", err)
	}
	list, _ = ts.List(17)
	if list[0].State != "done" || list[1].State != "pending" {
		t.Fatalf("states: %+v", list)
	}
}

func TestTodoStore_Errors(t *testing.T) {
	s, _ := OpenStore(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	ts := &TodoStore{Store: s}
	if _, err := ts.Add(999, "x"); err == nil {
		t.Error("add to nonexistent thread (FK) should error")
	}
	if err := ts.Update(12345, "done"); err == nil {
		t.Error("update unknown id should error")
	}
	if err := ts.Update(1, "bogus"); err == nil || err.Error() != "state must be pending|done" {
		t.Errorf("bad state: got %v", err)
	}
}

// TestTodos_AddIsIdempotent: re-adding the same item returns the original
// instead of a second row. An agent that loses its todo id re-adds verbatim
// and marks the copy done, which used to strand the first as forever-pending.
func TestTodos_AddIsIdempotent(t *testing.T) {
	s, _ := OpenStore(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.db.Exec(`INSERT INTO workspaces(id, name) VALUES(1, 'ws')`)
	s.db.Exec(`INSERT INTO channels(id, workspace_id, name, type) VALUES(1, 1, 'dm', 'dm')`)
	s.db.Exec(`INSERT INTO messages(id, channel_id, from_project, content, ts) VALUES(1, 1, 'a/b', 'task', datetime('now'))`)
	td := &TodoStore{Store: s}

	first, err := td.Add(1, "verify build")
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := td.Update(first.ID, "done"); err != nil {
		t.Fatalf("update: %v", err)
	}
	again, err := td.Add(1, "verify build")
	if err != nil {
		t.Fatalf("re-add: %v", err)
	}
	if again.ID != first.ID {
		t.Errorf("re-add created id %d, want the existing %d", again.ID, first.ID)
	}
	if again.State != "done" {
		t.Errorf("re-add reset state to %q, want the existing done", again.State)
	}
	list, _ := td.List(1)
	if len(list) != 1 {
		t.Fatalf("got %d todos, want 1", len(list))
	}

	// Distinct content still creates a distinct item.
	if _, err := td.Add(1, "run tests"); err != nil {
		t.Fatalf("add distinct: %v", err)
	}
	if list, _ = td.List(1); len(list) != 2 {
		t.Errorf("distinct content should add a row, got %d todos", len(list))
	}
}

func TestTodos_Delete(t *testing.T) {
	s, _ := OpenStore(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.db.Exec(`INSERT INTO workspaces(id, name) VALUES(1, 'ws')`)
	s.db.Exec(`INSERT INTO channels(id, workspace_id, name, type) VALUES(1, 1, 'dm', 'dm')`)
	s.db.Exec(`INSERT INTO messages(id, channel_id, from_project, content, ts) VALUES(1, 1, 'a/b', 'task', datetime('now'))`)
	td := &TodoStore{Store: s}

	keep, _ := td.Add(1, "keep me")
	drop, _ := td.Add(1, "stale orphan")
	if err := td.Delete(drop.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	list, _ := td.List(1)
	if len(list) != 1 || list[0].ID != keep.ID {
		t.Errorf("after delete got %+v, want only the kept item", list)
	}
	if err := td.Delete(drop.ID); err == nil {
		t.Error("deleting a missing todo should report it, not silently succeed")
	}
}
