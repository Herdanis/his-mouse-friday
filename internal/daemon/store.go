package daemon

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

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
	// finished_at lets callers show how long a task actually ran. Without it
	// elapsed is now-minus-start, which keeps climbing after the work is done.
	db.Exec(`ALTER TABLE sessions ADD COLUMN finished_at DATETIME`)
	// Backfill sessions that ended before the column existed, using the last
	// done reply on their thread as the finish time. Rows with no such reply
	// stay NULL — their duration is genuinely unknown, better than a guess.
	db.Exec(`UPDATE sessions SET finished_at = (
	           SELECT MAX(m.ts) FROM messages m
	           WHERE m.thread_id = sessions.root_thread_id AND m.status = 'done')
	         WHERE finished_at IS NULL
	           AND status IN ('exited','failed')
	           AND root_thread_id IS NOT NULL`)
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

// PruneResult counts what a Prune removed.
type PruneResult struct {
	Messages int64
	Sessions int64
	Todos    int64
	Skipped  int64 // threads left alone because they are still running
}

// ReapDeadSessions marks 'active' rows whose process is gone as exited. A
// daemon restart loses the exit watcher, so such a row would otherwise block
// deletes and wakes on its thread forever.
func (s *Store) ReapDeadSessions() error {
	rows, err := s.db.Query(`SELECT id, pid FROM sessions WHERE status='active'`)
	if err != nil {
		return err
	}
	var dead []int64
	for rows.Next() {
		var id int64
		var pid sql.NullInt64
		if err := rows.Scan(&id, &pid); err != nil {
			rows.Close()
			return err
		}
		// pid 0/NULL = spawn still in flight (Create precedes SetPID) — leave it.
		if pid.Valid && pid.Int64 > 0 && !processAlive(pid.Int64) {
			dead = append(dead, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	sessions := &SessionStore{Store: s}
	for _, id := range dead {
		if err := sessions.MarkExited(id, -1); err != nil {
			return err
		}
		logf("reap", "session %d marked exited: no live process", id)
	}
	return nil
}

// liveSessions names the still-running agents on a thread ("<name> pid N").
func (s *Store) liveSessions(tx *sql.Tx, root int64) ([]string, error) {
	rows, err := tx.Query(
		`SELECT IFNULL(name,'session '||id), IFNULL(pid,0) FROM sessions
		 WHERE status='active' AND root_thread_id=?`, root)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		var pid int64
		if err := rows.Scan(&name, &pid); err != nil {
			return nil, err
		}
		out = append(out, fmt.Sprintf("%s (pid %d)", name, pid))
	}
	return out, rows.Err()
}

// DeleteThread removes one task thread outright: its messages, sessions and
// todos. Refuses while an agent is still running on it — deleting then would
// orphan a live process whose replies have nowhere to land, the same
// invariant Prune holds.
func (s *Store) DeleteThread(root int64) (PruneResult, error) {
	var res PruneResult
	if err := s.ReapDeadSessions(); err != nil {
		return res, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return res, err
	}
	defer tx.Rollback()

	live, err := s.liveSessions(tx, root)
	if err != nil {
		return res, err
	}
	if len(live) > 0 {
		return res, fmt.Errorf("thread %d still has %d running agent(s): %s — wait for it, or kill the pid",
			root, len(live), strings.Join(live, ", "))
	}

	var msgs int64
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM messages WHERE id=? OR thread_id=?`, root, root).Scan(&msgs); err != nil {
		return res, err
	}
	if msgs == 0 {
		return res, fmt.Errorf("no such thread: %d", root)
	}

	del := func(query string, args ...any) (int64, error) {
		r, err := tx.Exec(query, args...)
		if err != nil {
			return 0, err
		}
		n, _ := r.RowsAffected()
		return n, nil
	}
	if res.Todos, err = del(`DELETE FROM todos WHERE thread_id=?`, root); err != nil {
		return res, err
	}
	if res.Sessions, err = del(`DELETE FROM sessions WHERE root_thread_id=?`, root); err != nil {
		return res, err
	}
	if res.Messages, err = del(`DELETE FROM messages WHERE id=? OR thread_id=?`, root, root); err != nil {
		return res, err
	}
	return res, tx.Commit()
}

// Prune deletes task history. olderThan == 0 removes everything; otherwise
// only threads whose last activity is older than that.
//
// Never touches workspaces or projects — the registry outlives history — and
// never removes a thread with a running session, which would orphan a live
// agent's replies.
func (s *Store) Prune(olderThan time.Duration) (PruneResult, error) {
	var res PruneResult
	if err := s.ReapDeadSessions(); err != nil {
		return res, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return res, err
	}
	defer tx.Rollback()

	// A thread is prunable when nothing on it is still running and its most
	// recent message is past the cutoff.
	cutoff := "1970-01-01"
	if olderThan > 0 {
		cutoff = time.Now().UTC().Add(-olderThan).Format("2006-01-02 15:04:05")
	}
	const liveThreads = `SELECT DISTINCT root_thread_id FROM sessions
	                     WHERE status='active' AND root_thread_id IS NOT NULL`
	keep := `SELECT root FROM (
	           SELECT IFNULL(thread_id, id) AS root, MAX(ts) AS last FROM messages GROUP BY root
	         ) WHERE last >= ? OR root IN (` + liveThreads + `)`

	if olderThan > 0 {
		tx.QueryRow(`SELECT COUNT(*) FROM (`+keep+`)`, cutoff).Scan(&res.Skipped)
	} else {
		tx.QueryRow(`SELECT COUNT(*) FROM (` + liveThreads + `)`).Scan(&res.Skipped)
		keep = liveThreads
	}

	del := func(query string, args ...any) (int64, error) {
		r, err := tx.Exec(query, args...)
		if err != nil {
			return 0, err
		}
		n, _ := r.RowsAffected()
		return n, nil
	}

	args := []any{}
	if olderThan > 0 {
		args = append(args, cutoff)
	}
	if res.Todos, err = del(`DELETE FROM todos WHERE thread_id NOT IN (`+keep+`)`, args...); err != nil {
		return res, err
	}
	if res.Sessions, err = del(
		`DELETE FROM sessions WHERE status != 'active'
		   AND (root_thread_id IS NULL OR root_thread_id NOT IN (`+keep+`))`, args...); err != nil {
		return res, err
	}
	if res.Messages, err = del(
		`DELETE FROM messages WHERE IFNULL(thread_id, id) NOT IN (`+keep+`)`, args...); err != nil {
		return res, err
	}
	if err := tx.Commit(); err != nil {
		return res, err
	}
	// Deleted pages stay allocated until reclaimed, so a prune alone leaves the
	// file its old size. VACUUM cannot run inside a transaction.
	s.db.Exec(`VACUUM`)
	return res, nil
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
