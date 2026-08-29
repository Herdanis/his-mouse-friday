package daemon

import (
	"path/filepath"
	"testing"
)

func TestOpenStore_CreatesSchema(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	tables := []string{"workspaces", "projects", "sessions", "channels", "messages"}
	for _, tbl := range tables {
		var name string
		err := s.db.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name=?", tbl).Scan(&name)
		if err != nil {
			t.Errorf("table %s missing: %v", tbl, err)
		}
	}
}

func TestStore_RetentionDelete(t *testing.T) {
	s, _ := OpenStore(filepath.Join(t.TempDir(), "test.db"))
	defer s.Close()

	// Insert a message dated 100 days ago.
	s.db.Exec(`INSERT INTO workspaces(id, name) VALUES(1, 'ws')`)
	s.db.Exec(`INSERT INTO channels(id, workspace_id, name, type) VALUES(1, 1, 'dm', 'dm')`)
	s.db.Exec(`INSERT INTO messages(channel_id, from_project, content, ts) VALUES(1, 'a/b', 'old', datetime('now','-100 days'))`)

	if err := s.RunRetention(); err != nil {
		t.Fatalf("retention: %v", err)
	}
	var n int
	s.db.QueryRow("SELECT count(*) FROM messages").Scan(&n)
	if n != 0 {
		t.Errorf("retention should delete old msg, got %d", n)
	}
}

// TestStore_FKEnforced proves foreign_keys is on for pooled connections, not
// just the one connection the old standalone PRAGMA landed on.
func TestStore_FKEnforced(t *testing.T) {
	s, _ := OpenStore(filepath.Join(t.TempDir(), "test.db"))
	defer s.Close()

	// Several inserts: pool may hand out different connections per query.
	for i := 0; i < 5; i++ {
		_, err := s.db.Exec(`INSERT INTO messages(channel_id, from_project, content, ts) VALUES(999, 'a/b', 'x', datetime('now'))`)
		if err == nil {
			t.Fatal("insert with bogus channel_id should violate FK")
		}
	}
}
