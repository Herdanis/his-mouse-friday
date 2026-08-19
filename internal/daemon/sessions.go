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
	RootThreadID   int64
	Prefix         string
}

type SessionStore struct {
	Store *Store
}

func (s *SessionStore) Create(projectID int64, binary, model string, pid int, taskMsgID, rootThreadID int64, prefix, name string) (Session, error) {
	now := time.Now().UTC()
	res, err := s.Store.db.Exec(
		`INSERT INTO sessions(project_id, agent_binary, model, status, pid, created_at, task_msg_id, root_thread_id, prefix, name)
		 VALUES(?,?,?,?,?,?,?,?,?,?)`,
		projectID, binary, model, "active", pid, now, nullIfZero(taskMsgID), nullIfZero(rootThreadID), nullIfEmpty(prefix), nullIfEmpty(name))
	if err != nil {
		return Session{}, err
	}
	id, _ := res.LastInsertId()
	return Session{
		ID: id, ProjectID: projectID, AgentBinary: binary, Model: model,
		Status: "active", PID: pid, CreatedAt: now,
		RootThreadID: rootThreadID, Prefix: prefix, Name: name,
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

// MarkExited records that the spawned agent's process exited. Exit code 0 =>
// status "exited" (clean); non-zero => "failed". Lets task_status distinguish
// "still working" from "agent died without replying".
// nullIfEmpty returns nil for "" so sql.Exec inserts NULL for optional text
// fields (prefix, name). Mirrors nullIfZero for int64 FK columns.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// SetAgentSessionID binds the agent runtime session id (captured post-spawn)
// to a hmf session row so later wakes can resume it. DB column stays
// opencode_session_id — migration cost > benefit.
func (s *SessionStore) SetAgentSessionID(id int64, ocID string) error {
	_, err := s.Store.db.Exec(`UPDATE sessions SET opencode_session_id=? WHERE id=?`, ocID, id)
	return err
}

func (s *SessionStore) MarkExited(id int64, exitCode int) error {
	status := "exited"
	if exitCode != 0 {
		status = "failed"
	}
	_, err := s.Store.db.Exec(`UPDATE sessions SET status=?, exit_code=? WHERE id=?`, status, exitCode, id)
	return err
}
