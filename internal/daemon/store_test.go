package daemon

import (
	"path/filepath"
	"testing"
	"time"
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

// TestStore_Prune covers the two things a prune must never do: drop the
// registry, or delete a thread whose agent is still running.
func TestStore_Prune(t *testing.T) {
	s, _ := OpenStore(filepath.Join(t.TempDir(), "test.db"))
	defer s.Close()

	s.db.Exec(`INSERT INTO workspaces(id, name) VALUES(1, 'ws')`)
	s.db.Exec(`INSERT INTO projects(id, workspace_id, name, path) VALUES(1, 1, 'p', '/tmp/p')`)
	s.db.Exec(`INSERT INTO channels(id, workspace_id, name, type) VALUES(1, 1, 'dm', 'dm')`)

	// Thread 1: finished. Thread 3: still has an active session.
	s.db.Exec(`INSERT INTO messages(id, channel_id, from_project, content, ts) VALUES(1, 1, 'a/b', 'done task', datetime('now'))`)
	s.db.Exec(`INSERT INTO messages(id, channel_id, thread_id, from_project, content, ts) VALUES(2, 1, 1, 'a/b', 'reply', datetime('now'))`)
	s.db.Exec(`INSERT INTO messages(id, channel_id, from_project, content, ts) VALUES(3, 1, 'a/b', 'live task', datetime('now'))`)
	s.db.Exec(`INSERT INTO todos(thread_id, content, updated_at) VALUES(1, 'old', datetime('now'))`)
	s.db.Exec(`INSERT INTO todos(thread_id, content, updated_at) VALUES(3, 'live', datetime('now'))`)
	s.db.Exec(`INSERT INTO sessions(project_id, agent_binary, status, created_at, root_thread_id) VALUES(1, 'opencode', 'exited', datetime('now'), 1)`)
	s.db.Exec(`INSERT INTO sessions(project_id, agent_binary, status, created_at, root_thread_id) VALUES(1, 'opencode', 'active', datetime('now'), 3)`)

	res, err := s.Prune(0)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if res.Messages != 2 || res.Sessions != 1 || res.Todos != 1 {
		t.Errorf("counts = %+v, want 2 messages / 1 session / 1 todo", res)
	}

	var msgs, todos, sessions, projects int
	s.db.QueryRow("SELECT count(*) FROM messages").Scan(&msgs)
	s.db.QueryRow("SELECT count(*) FROM todos").Scan(&todos)
	s.db.QueryRow("SELECT count(*) FROM sessions").Scan(&sessions)
	s.db.QueryRow("SELECT count(*) FROM projects").Scan(&projects)
	if msgs != 1 || todos != 1 || sessions != 1 {
		t.Errorf("live thread not preserved: %d msgs, %d todos, %d sessions", msgs, todos, sessions)
	}
	if projects != 1 {
		t.Errorf("prune deleted the registry: %d projects left", projects)
	}
}

// TestStore_PruneOlderThan keeps recent threads even when nothing is running.
func TestStore_PruneOlderThan(t *testing.T) {
	s, _ := OpenStore(filepath.Join(t.TempDir(), "test.db"))
	defer s.Close()

	s.db.Exec(`INSERT INTO workspaces(id, name) VALUES(1, 'ws')`)
	s.db.Exec(`INSERT INTO channels(id, workspace_id, name, type) VALUES(1, 1, 'dm', 'dm')`)
	s.db.Exec(`INSERT INTO messages(id, channel_id, from_project, content, ts) VALUES(1, 1, 'a/b', 'old', datetime('now','-5 days'))`)
	s.db.Exec(`INSERT INTO messages(id, channel_id, from_project, content, ts) VALUES(2, 1, 'a/b', 'recent', datetime('now'))`)

	if _, err := s.Prune(24 * time.Hour); err != nil {
		t.Fatalf("prune: %v", err)
	}
	var content string
	if err := s.db.QueryRow("SELECT content FROM messages").Scan(&content); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if content != "recent" {
		t.Errorf("kept %q, want the recent message", content)
	}
}
