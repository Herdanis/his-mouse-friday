package daemon

import (
	"database/sql"
	"time"
)

type Session struct {
	ID          int64
	ProjectID   int64
	AgentBinary string
	Model       string
	Status      string
	PID         int
	CreatedAt   time.Time
	// Resume feature: bind spawned agent runtime session to a thread root for
	// resume, plus human-friendly name + random prefix for tracing siblings.
	AgentSessionID string
	Name           string
	ParentID       int64
	Prefix         string
}

type SessionStore struct {
	Store *Store
}

func (s *SessionStore) Create(projectID int64, binary, model string, pid int, taskMsgID, parentID int64, prefix, name string) (Session, error) {
	now := time.Now().UTC()
	res, err := s.Store.db.Exec(
		`INSERT INTO sessions(project_id, agent_binary, model, status, pid, created_at, task_msg_id, root_thread_id, prefix, name)
		 VALUES(?,?,?,?,?,?,?,?,?,?)`,
		projectID, binary, model, "active", pid, now, nullIfZero(taskMsgID), nullIfZero(parentID), nullIfEmpty(prefix), nullIfEmpty(name))
	if err != nil {
		return Session{}, err
	}
	id, _ := res.LastInsertId()
	return Session{
		ID: id, ProjectID: projectID, AgentBinary: binary, Model: model,
		Status: "active", PID: pid, CreatedAt: now,
		ParentID: parentID, Prefix: prefix, Name: name,
	}, nil
}

func (s *SessionStore) Get(id int64) (Session, error) {
	var sess Session
	var pid sql.NullInt64
	err := s.Store.db.QueryRow(
		`SELECT id, project_id, agent_binary, model, status, pid, created_at FROM sessions WHERE id=?`, id).
		Scan(&sess.ID, &sess.ProjectID, &sess.AgentBinary, &sess.Model, &sess.Status, &pid, &sess.CreatedAt)
	if err == sql.ErrNoRows {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, err
	}
	sess.PID = int(pid.Int64)
	return sess, nil
}

func (s *SessionStore) SetStatus(id int64, status string) error {
	_, err := s.Store.db.Exec(`UPDATE sessions SET status=? WHERE id=?`, status, id)
	return err
}

func (s *SessionStore) SetPID(id int64, pid int) error {
	_, err := s.Store.db.Exec(`UPDATE sessions SET pid=? WHERE id=?`, pid, id)
	return err
}

// nullIfEmpty returns nil for "" so sql.Exec inserts NULL for optional text
// fields (prefix, name). Mirrors nullIfZero for int64 FK columns.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// SetAgentSessionID binds the captured opencode session id to a hmf session
// row so later wakes can resume it. Column name stays opencode_session_id.
func (s *SessionStore) SetAgentSessionID(id int64, ocID string) error {
	_, err := s.Store.db.Exec(`UPDATE sessions SET opencode_session_id=? WHERE id=?`, ocID, id)
	return err
}

// MarkExited: exit code 0 → "exited", non-zero → "failed".
func (s *SessionStore) MarkExited(id int64, exitCode int) error {
	status := "exited"
	if exitCode != 0 {
		status = "failed"
	}
	// Stamp the finish time so elapsed can freeze at the real duration.
	// COALESCE keeps the first one: a re-marked session isn't running longer.
	_, err := s.Store.db.Exec(
		`UPDATE sessions SET status=?, exit_code=?, finished_at=COALESCE(finished_at, ?) WHERE id=?`,
		status, exitCode, time.Now().UTC(), id)
	return err
}
