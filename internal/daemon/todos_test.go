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
