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
}

type SessionStore struct {
	Store *Store
}

func (s *SessionStore) Create(projectID int64, binary, model string, pid int, taskMsgID int64) (Session, error) {
	now := time.Now().UTC()
	res, err := s.Store.db.Exec(
		`INSERT INTO sessions(project_id, agent_binary, model, status, pid, created_at, task_msg_id)
		 VALUES(?,?,?,?,?,?,?)`,
		projectID, binary, model, "active", pid, now, nullIfZeroInt(taskMsgID))
	if err != nil {
		return Session{}, err
	}
	id, _ := res.LastInsertId()
	return Session{ID: id, ProjectID: projectID, AgentBinary: binary, Model: model, Status: "active", PID: pid, CreatedAt: now}, nil
}

func nullIfZeroInt(i int64) any {
	if i == 0 {
		return nil
	}
	return i
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
func (s *SessionStore) MarkExited(id int64, exitCode int) error {
	status := "exited"
	if exitCode != 0 {
		status = "failed"
	}
	_, err := s.Store.db.Exec(`UPDATE sessions SET status=?, exit_code=? WHERE id=?`, status, exitCode, id)
	return err
}
