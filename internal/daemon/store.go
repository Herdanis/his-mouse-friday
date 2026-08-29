package daemon

import (
	"database/sql"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS workspaces (
  id   INTEGER PRIMARY KEY,
  name TEXT NOT NULL UNIQUE
);
CREATE TABLE IF NOT EXISTS projects (
  id           INTEGER PRIMARY KEY,
  workspace_id INTEGER NOT NULL REFERENCES workspaces(id),
  name         TEXT NOT NULL,
  path         TEXT NOT NULL,
  UNIQUE(workspace_id, name)
);
CREATE TABLE IF NOT EXISTS sessions (
  id           INTEGER PRIMARY KEY,
  project_id   INTEGER NOT NULL REFERENCES projects(id),
  agent_binary TEXT NOT NULL,
  model        TEXT,
  status       TEXT NOT NULL,
  pid          INTEGER,
  created_at   DATETIME NOT NULL
);
CREATE TABLE IF NOT EXISTS channels (
  id           INTEGER PRIMARY KEY,
  workspace_id INTEGER NOT NULL REFERENCES workspaces(id),
  name         TEXT NOT NULL,
  type         TEXT NOT NULL,
  UNIQUE(workspace_id, name)
);
CREATE TABLE IF NOT EXISTS messages (
  id           INTEGER PRIMARY KEY,
  channel_id   INTEGER NOT NULL REFERENCES channels(id),
  thread_id    INTEGER REFERENCES messages(id),
  from_project TEXT NOT NULL,
  to_project   TEXT,
  content      TEXT NOT NULL,
  status       TEXT NOT NULL DEFAULT 'message',
  ts           DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_messages_channel_ts ON messages(channel_id, ts);
CREATE INDEX IF NOT EXISTS idx_messages_thread ON messages(thread_id);
CREATE TABLE IF NOT EXISTS todos (
  id         INTEGER PRIMARY KEY,
  thread_id  INTEGER NOT NULL REFERENCES messages(id),
  content    TEXT NOT NULL,
  state      TEXT NOT NULL DEFAULT 'pending',
  updated_at DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_todos_thread ON todos(thread_id);
`

func OpenStore(path string) (*Store, error) {
	// Pragmas via DSN so every pooled connection gets them (per-connection
	// PRAGMAs via db.Exec only stick to one conn). 5s busy_timeout is plenty
	// for hmf's low concurrency.
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	// Migrations (idempotent — ALTERs no-op if column exists).
	db.Exec(`ALTER TABLE messages ADD COLUMN status TEXT NOT NULL DEFAULT 'message'`)
	db.Exec(`ALTER TABLE sessions ADD COLUMN task_msg_id INTEGER`)
	db.Exec(`ALTER TABLE sessions ADD COLUMN exit_code INTEGER`)
	db.Exec(`ALTER TABLE sessions ADD COLUMN opencode_session_id TEXT`)
	db.Exec(`ALTER TABLE sessions ADD COLUMN name TEXT`)
	db.Exec(`ALTER TABLE sessions ADD COLUMN root_thread_id INTEGER`)
	db.Exec(`ALTER TABLE sessions ADD COLUMN prefix TEXT`)
	// Global "general" channel — lobby where all agents live. Sentinel
	// __global__ workspace satisfies the FK without a schema migration.
	db.Exec(`INSERT OR IGNORE INTO workspaces(name) VALUES('__global__')`)
	db.Exec(`INSERT OR IGNORE INTO channels(workspace_id, name, type)
	         VALUES((SELECT id FROM workspaces WHERE name='__global__'), 'general', 'group')`)
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// nullIfZero returns nil for 0 so a sql.Exec inserts NULL for FK columns
// (thread_id, task_msg_id) that are optional. Used by Comms + SessionStore.
func nullIfZero(i int64) any {
	if i == 0 {
		return nil
	}
	return i
}

// RunRetention deletes messages older than 90 days. Runs in a transaction:
// todos.thread_id is a foreign key onto messages.id, and an unscoped DELETE
// aborts entirely on the first FK violation it hits — so a message's todos
// are cleared first.
func (s *Store) RunRetention() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	const cutoff = `ts < datetime('now', '-90 days')`
	if _, err := tx.Exec(`DELETE FROM todos WHERE thread_id IN (SELECT id FROM messages WHERE ` + cutoff + `)`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM messages WHERE ` + cutoff); err != nil {
		return err
	}
	return tx.Commit()
}
