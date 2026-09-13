package daemon

import (
	"database/sql"
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

	tables := []string{"projects", "sessions", "channels", "messages"}
	for _, tbl := range tables {
		var name string
		err := s.db.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name=?", tbl).Scan(&name)
		if err != nil {
			t.Errorf("table %s missing: %v", tbl, err)
		}
	}
	var name string
	err = s.db.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name='workspaces'").Scan(&name)
	if err == nil {
		t.Error("workspaces table must not exist in a new database")
	}
}

func TestStore_RetentionDelete(t *testing.T) {
	s, _ := OpenStore(filepath.Join(t.TempDir(), "test.db"))
	defer s.Close()

	// Insert a message dated 100 days ago.
	s.db.Exec(`INSERT INTO channels(id, name, type) VALUES(1, 'dm', 'dm')`)
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

	s.db.Exec(`INSERT INTO projects(id, name, path) VALUES(1, 'p', '/tmp/p')`)
	s.db.Exec(`INSERT INTO channels(id, name, type) VALUES(1, 'dm', 'dm')`)

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

	s.db.Exec(`INSERT INTO channels(id, name, type) VALUES(1, 'dm', 'dm')`)
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

// ============================================
// DeleteThread
// ============================================

func deleteThreadFixture(t *testing.T) *Store {
	t.Helper()
	s, _ := OpenStore(filepath.Join(t.TempDir(), "test.db"))
	t.Cleanup(func() { s.Close() })
	s.db.Exec(`INSERT INTO projects(id, name, path) VALUES(1, 'p', '/tmp/p')`)
	s.db.Exec(`INSERT INTO channels(id, name, type) VALUES(1, 'dm', 'dm')`)
	// Thread 1: finished, one reply, one todo, one exited session.
	// Thread 3: unrelated, must survive.
	s.db.Exec(`INSERT INTO messages(id, channel_id, from_project, content, ts) VALUES(1, 1, 'a/b', 'drop me', datetime('now'))`)
	s.db.Exec(`INSERT INTO messages(id, channel_id, thread_id, from_project, content, ts) VALUES(2, 1, 1, 'a/b', 'reply', datetime('now'))`)
	s.db.Exec(`INSERT INTO messages(id, channel_id, from_project, content, ts) VALUES(3, 1, 'a/b', 'keep me', datetime('now'))`)
	s.db.Exec(`INSERT INTO todos(thread_id, content, updated_at) VALUES(1, 'gone', datetime('now'))`)
	s.db.Exec(`INSERT INTO todos(thread_id, content, updated_at) VALUES(3, 'stays', datetime('now'))`)
	s.db.Exec(`INSERT INTO sessions(project_id, agent_binary, status, created_at, root_thread_id) VALUES(1, 'opencode', 'exited', datetime('now'), 1)`)
	return s
}

func TestStore_DeleteThread(t *testing.T) {
	s := deleteThreadFixture(t)

	res, err := s.DeleteThread(1)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if res.Messages != 2 || res.Sessions != 1 || res.Todos != 1 {
		t.Errorf("counts = %+v, want 2 messages / 1 session / 1 todo", res)
	}

	var msgs, todos, sessions, projects int
	s.db.QueryRow("SELECT count(*) FROM messages").Scan(&msgs)
	s.db.QueryRow("SELECT count(*) FROM todos").Scan(&todos)
	s.db.QueryRow("SELECT count(*) FROM sessions").Scan(&sessions)
	s.db.QueryRow("SELECT count(*) FROM projects").Scan(&projects)
	if msgs != 1 || todos != 1 || sessions != 0 {
		t.Errorf("unrelated thread not preserved: %d msgs, %d todos, %d sessions", msgs, todos, sessions)
	}
	if projects != 1 {
		t.Errorf("delete took out the registry: %d projects left", projects)
	}
}

// Deleting a thread with a live agent would orphan the process — its replies
// would have no thread to land on. Same invariant Prune holds.
func TestStore_DeleteThreadRefusesWhileAgentRuns(t *testing.T) {
	s := deleteThreadFixture(t)
	s.db.Exec(`INSERT INTO sessions(project_id, agent_binary, status, created_at, root_thread_id) VALUES(1, 'opencode', 'active', datetime('now'), 1)`)

	if _, err := s.DeleteThread(1); err == nil {
		t.Fatal("deleted a thread with a running agent")
	}
	var msgs int
	s.db.QueryRow("SELECT count(*) FROM messages").Scan(&msgs)
	if msgs != 3 {
		t.Errorf("refused delete still removed rows: %d messages left, want 3", msgs)
	}
}

// An 'active' row left behind by a daemon restart (pid long dead) must not
// block the delete forever.
func TestStore_DeleteThreadReapsDeadSession(t *testing.T) {
	s := deleteThreadFixture(t)
	deadPID := 0x7FFFFFF0 // out of pid range: guaranteed not running
	s.db.Exec(`INSERT INTO sessions(project_id, agent_binary, status, pid, created_at, root_thread_id) VALUES(1, 'opencode', 'active', ?, datetime('now'), 1)`, deadPID)

	if _, err := s.DeleteThread(1); err != nil {
		t.Fatalf("delete blocked by a dead session: %v", err)
	}
	var msgs int
	s.db.QueryRow("SELECT count(*) FROM messages").Scan(&msgs)
	if msgs != 1 {
		t.Errorf("%d messages left, want 1 (the unrelated thread)", msgs)
	}
}

func TestStore_DeleteThreadUnknown(t *testing.T) {
	if _, err := deleteThreadFixture(t).DeleteThread(9999); err == nil {
		t.Fatal("want an error for a thread that does not exist")
	}
}

// ============================================
// Workspace schema migration
// ============================================

// oldSchemaDB builds a pre-bare-name database by hand: workspaces table,
// projects with workspace_id, channels with the workspace FK, and message
// identities in "ws/name" form.
func oldSchemaDB(t *testing.T) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const oldSchema = `
	CREATE TABLE workspaces (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE);
	CREATE TABLE projects (
	  id INTEGER PRIMARY KEY,
	  workspace_id INTEGER NOT NULL REFERENCES workspaces(id),
	  name TEXT NOT NULL, path TEXT NOT NULL,
	  UNIQUE(workspace_id, name));
	CREATE TABLE sessions (
	  id INTEGER PRIMARY KEY,
	  project_id INTEGER NOT NULL REFERENCES projects(id),
	  agent_binary TEXT NOT NULL, model TEXT, status TEXT NOT NULL,
	  pid INTEGER, created_at DATETIME NOT NULL);
	CREATE TABLE channels (
	  id INTEGER PRIMARY KEY,
	  workspace_id INTEGER NOT NULL REFERENCES workspaces(id),
	  name TEXT NOT NULL, type TEXT NOT NULL,
	  UNIQUE(workspace_id, name));
	CREATE TABLE messages (
	  id INTEGER PRIMARY KEY,
	  channel_id INTEGER NOT NULL REFERENCES channels(id),
	  thread_id INTEGER REFERENCES messages(id),
	  from_project TEXT NOT NULL, to_project TEXT, content TEXT NOT NULL,
	  status TEXT NOT NULL DEFAULT 'message', ts DATETIME NOT NULL);
	CREATE TABLE todos (
	  id INTEGER PRIMARY KEY,
	  thread_id INTEGER NOT NULL REFERENCES messages(id),
	  content TEXT NOT NULL, state TEXT NOT NULL DEFAULT 'pending',
	  updated_at DATETIME NOT NULL);`
	for _, q := range []string{oldSchema,
		`INSERT INTO workspaces(id, name) VALUES(1, 'co'), (2, 'personal')`,
		// Duplicate name across workspaces: the later row (id 2) keeps the
		// bare name, the older one is renamed parent-2.
		`INSERT INTO projects(id, workspace_id, name, path) VALUES(1, 1, 'parent', '/p1'),
		                                                                 (2, 2, 'parent', '/p2'),
		                                                                 (3, 1, 'child', '/c')`,
		`INSERT INTO channels(id, workspace_id, name, type) VALUES(1, 1, 'general', 'group')`,
		`INSERT INTO messages(id, channel_id, from_project, to_project, content, ts)
		 VALUES(1, 1, 'co/parent', 'personal/child', 'task', datetime('now')),
		       (2, 1, 'ghost/parent', 'co/child', 'task2', datetime('now'))`,
		`INSERT INTO sessions(project_id, agent_binary, status, created_at)
		 VALUES(1, 'opencode', 'exited', datetime('now'))`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	return dbPath
}

func TestStore_MigratesWorkspaceSchema(t *testing.T) {
	dbPath := oldSchemaDB(t)
	s, err := OpenStore(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	// Projects survive with the same ids; the older duplicate got renamed.
	var name1, name2, name3 string
	if err := s.db.QueryRow(`SELECT name FROM projects WHERE id=1`).Scan(&name1); err != nil {
		t.Fatalf("project 1 lost: %v", err)
	}
	s.db.QueryRow(`SELECT name FROM projects WHERE id=2`).Scan(&name2)
	s.db.QueryRow(`SELECT name FROM projects WHERE id=3`).Scan(&name3)
	if name1 != "parent-2" || name2 != "parent" || name3 != "child" {
		t.Errorf("renames wrong: id1=%q id2=%q id3=%q", name1, name2, name3)
	}

	// workspaces table dropped.
	var tbl string
	if err := s.db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='workspaces'`).Scan(&tbl); err == nil {
		t.Error("workspaces table still exists after migration")
	}

	// Message identities: real workspace prefixes stripped, others kept.
	var from1, to1, from2, to2 string
	s.db.QueryRow(`SELECT from_project, to_project FROM messages WHERE id=1`).Scan(&from1, &to1)
	s.db.QueryRow(`SELECT from_project, to_project FROM messages WHERE id=2`).Scan(&from2, &to2)
	if from1 != "parent" || to1 != "child" {
		t.Errorf("msg1 identities = %q → %q, want parent → child", from1, to1)
	}
	if from2 != "ghost/parent" || to2 != "child" {
		t.Errorf("msg2 identities = %q → %q, want ghost/parent kept → child", from2, to2)
	}

	// Sessions rows intact.
	var sessN int
	s.db.QueryRow(`SELECT count(*) FROM sessions`).Scan(&sessN)
	if sessN != 1 {
		t.Errorf("%d sessions after migration, want 1", sessN)
	}

	// FK integrity: a new session for a migrated project must insert cleanly.
	ss := &SessionStore{Store: s}
	if _, err := ss.Create(3, "opencode", "default", 0, 0, 0, "abc12", "abc12-child"); err != nil {
		t.Errorf("insert into migrated schema failed (FK not retargeted?): %v", err)
	}
}

// Reopening an already-migrated database must be a no-op, not a second pass.
func TestStore_MigrationIdempotent(t *testing.T) {
	dbPath := oldSchemaDB(t)
	s1, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	s1.Close()
	s2, err := OpenStore(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	var n int
	if err := s2.db.QueryRow(`SELECT count(*) FROM projects`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("%d projects after reopen, want 3", n)
	}
}
