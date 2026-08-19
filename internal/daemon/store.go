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
`

func OpenStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// busy_timeout: concurrent writers (e.g. wakeAgent's OnExit goroutine
	// posting a synthetic BLOCKED reply while a read_channel request runs)
	// wait up to 5s instead of failing immediately with SQLITE_BUSY.
	if _, err := db.Exec(`PRAGMA busy_timeout=5000`); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(`PRAGMA foreign_keys=ON`); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	// Migrate: add status column if missing (pre-existing DBs).
	db.Exec(`ALTER TABLE messages ADD COLUMN status TEXT NOT NULL DEFAULT 'message'`)
	// Migrate: link sessions to the task message they were spawned for, + track
	// exit code so task_status can tell "still working" from "agent died".
	db.Exec(`ALTER TABLE sessions ADD COLUMN task_msg_id INTEGER`)
	db.Exec(`ALTER TABLE sessions ADD COLUMN exit_code INTEGER`)
	// Ensure the global "general" channel exists — the single lobby where all
	// agents live. Uses a sentinel __global__ workspace so the channels.workspace_id
	// FK is satisfied without a schema migration.
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

// RunRetention deletes messages older than 90 days.
func (s *Store) RunRetention() error {
	_, err := s.db.Exec(`DELETE FROM messages WHERE ts < datetime('now', '-90 days')`)
	return err
}
