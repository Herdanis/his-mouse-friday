package daemon

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS projects (
  id   INTEGER PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  path TEXT NOT NULL
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
  id   INTEGER PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  type TEXT NOT NULL
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
	// Migrate the pre-bare-name schema (workspaces + workspace_id FKs) before
	// the schema exec: migrated tables make it a no-op, fresh DBs never had
	// the old shape so migration skips.
	if err := migrateWorkspaces(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("workspace migration: %w", err)
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
	// Global "general" channel — lobby where all agents live.
	db.Exec(`INSERT OR IGNORE INTO channels(name, type) VALUES('general', 'group')`)
	return &Store{db: db}, nil
}

// migrateWorkspaces rebuilds a pre-bare-name database: projects keyed by
// (workspace_id, name) become top-level UNIQUE(name), channels lose their
// workspace FK, and "ws/name" message identities lose the ws/ prefix when ws
// was a real workspace. Skips when projects has no workspace_id column, so it
// is idempotent and cheap for new databases.
func migrateWorkspaces(db *sql.DB) error {
	ctx := context.Background()
	// Every statement runs on one dedicated connection: foreign_keys and
	// legacy_alter_table are connection-scoped PRAGMAs, and database/sql
	// pooling would otherwise land them on a different connection than the
	// migration statements.
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	var old int
	if err := conn.QueryRowContext(ctx,
		`SELECT count(*) FROM pragma_table_info('projects') WHERE name='workspace_id'`).Scan(&old); err != nil {
		return err
	}
	if old == 0 {
		return nil
	}
	// foreign_keys is a no-op inside a transaction (sqlite silently ignores
	// it there), so FKs go off here, before the migration tx opens.
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
		return err
	}
	defer func() {
		conn.ExecContext(ctx, `PRAGMA foreign_keys=ON`)
		conn.ExecContext(ctx, `PRAGMA legacy_alter_table=OFF`)
	}()
	// legacy_alter_table: without it, renaming projects rewrites sibling FK
	// clauses (sessions REFERENCES projects → projects_old), orphaning the
	// rebuilt tables from their foreign keys. ON keeps the rename byte-only.
	if _, err := conn.ExecContext(ctx, `PRAGMA legacy_alter_table=ON`); err != nil {
		return err
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`ALTER TABLE projects RENAME TO projects_old`,
		`ALTER TABLE channels RENAME TO channels_old`,
		`CREATE TABLE projects (
		  id   INTEGER PRIMARY KEY,
		  name TEXT NOT NULL UNIQUE,
		  path TEXT NOT NULL)`,
		`CREATE TABLE channels (
		  id   INTEGER PRIMARY KEY,
		  name TEXT NOT NULL UNIQUE,
		  type TEXT NOT NULL)`,
	} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	// Most recent registration (highest id) keeps the bare name; older
	// duplicates get renamed name-2, name-3… with a warning.
	if err := copyRenamed(ctx, tx, "projects_old", "projects", "path"); err != nil {
		return err
	}
	if err := copyRenamed(ctx, tx, "channels_old", "channels", "type"); err != nil {
		return err
	}
	// Strip "ws/" from message identities, but only where ws was a real
	// workspace — "human/scratch" style values were never workspaces.
	wsNames, err := func() ([]string, error) {
		rows, err := tx.QueryContext(ctx, `SELECT name FROM workspaces`)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var names []string
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				return nil, err
			}
			names = append(names, n)
		}
		return names, rows.Err()
	}()
	if err != nil {
		return err
	}
	for _, ws := range wsNames {
		// substr is 1-based: skip len(ws)+1 chars ("ws/"), +1 for the base.
		off := len(ws) + 2
		prefix := ws + "/%"
		if _, err := tx.ExecContext(ctx,
			`UPDATE messages SET from_project=substr(from_project, ?) WHERE from_project LIKE ?`,
			off, prefix); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE messages SET to_project=substr(to_project, ?) WHERE to_project LIKE ?`,
			off, prefix); err != nil {
			return err
		}
	}
	for _, q := range []string{
		`DROP TABLE projects_old`,
		`DROP TABLE channels_old`,
		`DROP TABLE workspaces`,
	} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	// FKs are back on (deferred pragma); surface any violation the copy
	// step could have introduced instead of failing silently later.
	rows, err := conn.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return err
	}
	defer rows.Close()
	if rows.Next() {
		var table string
		var rowid, parent, fkid int64
		rows.Scan(&table, &rowid, &parent, &fkid)
		logErrf("migrate", "workspace migration left FK violations (first: %s row %d)", table, rowid)
	}
	return nil
}

// copyRenamed copies (id, name, cols…) from oldTable to newTable. On a
// duplicate name the OLDER row (lower id) is renamed name-2, name-3… so the
// most recent registration keeps the bare name; a rename that would collide
// with an existing name skips to the next free suffix. Every rename warns.
func copyRenamed(ctx context.Context, tx *sql.Tx, oldTable, newTable string, cols ...string) error {
	all := append([]string{"id", "name"}, cols...)
	rows, err := tx.QueryContext(ctx,
		`SELECT `+strings.Join(all, ",")+` FROM `+oldTable+` ORDER BY id DESC`)
	if err != nil {
		return err
	}
	type rec struct {
		id   int64
		name string
		vals []sql.NullString
	}
	var recs []rec
	for rows.Next() {
		var r rec
		r.vals = make([]sql.NullString, len(cols))
		dest := []any{&r.id, &r.name}
		for i := range r.vals {
			dest = append(dest, &r.vals[i])
		}
		if err := rows.Scan(dest...); err != nil {
			rows.Close()
			return err
		}
		recs = append(recs, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	seen := map[string]bool{}
	nextSuffix := map[string]int{}
	ins := `INSERT INTO ` + newTable + `(` + strings.Join(all, ",") + `)
	        VALUES(` + strings.TrimSuffix(strings.Repeat("?,", len(all)), ",") + `)`
	for _, r := range recs {
		name := r.name
		if seen[name] {
			n := nextSuffix[name]
			for {
				n++
				if !seen[name+"-"+strconv.Itoa(n)] {
					break
				}
			}
			nextSuffix[name] = n
			renamed := name + "-" + strconv.Itoa(n)
			logf("migrate", "duplicate name %q in %s (older row id=%d) renamed to %q", r.name, newTable, r.id, renamed)
			name = renamed
		} else {
			nextSuffix[name] = 1
		}
		seen[name] = true
		args := []any{r.id, name}
		for _, v := range r.vals {
			args = append(args, v.String)
		}
		if _, err := tx.ExecContext(ctx, ins, args...); err != nil {
			return err
		}
	}
	return nil
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
