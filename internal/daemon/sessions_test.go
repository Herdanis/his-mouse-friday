package daemon

import (
	"database/sql"
	"testing"
)

func TestSessionStore_CreateAndGet(t *testing.T) {
	r := &Registry{Store: newTestStore(t)}
	r.AddWorkspace("companyA")
	r.AddProject("companyA", "payment-service", "/tmp/payment")

	ss := &SessionStore{Store: r.Store}
	s, err := ss.Create(1, "opencode", "default", 12345, 100)
	if err != nil {
		t.Fatal(err)
	}
	if s.ID == 0 || s.Status != "active" {
		t.Errorf("got %+v", s)
	}
	got, err := ss.Get(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.PID != 12345 || got.AgentBinary != "opencode" {
		t.Errorf("got %+v", got)
	}
}

func TestSessionStore_SetStatus(t *testing.T) {
	store := newTestStore(t)
	store.db.Exec(`INSERT INTO workspaces(id, name) VALUES(1, 'ws')`)
	store.db.Exec(`INSERT INTO projects(id, workspace_id, name, path) VALUES(1, 1, 'p', '/tmp/p')`)
	ss := &SessionStore{Store: store}
	s, _ := ss.Create(1, "opencode", "default", 99, 200)
	if err := ss.SetStatus(s.ID, "failed"); err != nil {
		t.Fatal(err)
	}
	got, _ := ss.Get(s.ID)
	if got.Status != "failed" {
		t.Errorf("status: got %q want failed", got.Status)
	}
}

// Create stores task_msg_id so task_status can link a session back to its task.
func TestSessionStore_CreateStoresTaskMsgID(t *testing.T) {
	store := newTestStore(t)
	store.db.Exec(`INSERT INTO workspaces(id, name) VALUES(1, 'ws')`)
	store.db.Exec(`INSERT INTO projects(id, workspace_id, name, path) VALUES(1, 1, 'p', '/tmp/p')`)
	ss := &SessionStore{Store: store}
	s, _ := ss.Create(1, "opencode", "default", 0, 4242)
	var taskMsgID int64
	store.db.QueryRow(`SELECT task_msg_id FROM sessions WHERE id=?`, s.ID).Scan(&taskMsgID)
	if taskMsgID != 4242 {
		t.Fatalf("task_msg_id: got %d want 4242", taskMsgID)
	}
}

// Create with task_msg_id=0 stores NULL (a reply session, not tied to a task).
func TestSessionStore_CreateZeroTaskMsgIDStoresNull(t *testing.T) {
	store := newTestStore(t)
	store.db.Exec(`INSERT INTO workspaces(id, name) VALUES(1, 'ws')`)
	store.db.Exec(`INSERT INTO projects(id, workspace_id, name, path) VALUES(1, 1, 'p', '/tmp/p')`)
	ss := &SessionStore{Store: store}
	s, _ := ss.Create(1, "opencode", "default", 0, 0)
	var nullable sql.NullInt64
	store.db.QueryRow(`SELECT task_msg_id FROM sessions WHERE id=?`, s.ID).Scan(&nullable)
	if nullable.Valid {
		t.Fatalf("task_msg_id: got %d want NULL", nullable.Int64)
	}
}

// MarkExited: exit code 0 => status "exited"; non-zero => "failed". Guards
// the orchestrator's "agent died vs clean exit" distinction.
func TestSessionStore_MarkExited_CleanVsFailed(t *testing.T) {
	store := newTestStore(t)
	store.db.Exec(`INSERT INTO workspaces(id, name) VALUES(1, 'ws')`)
	store.db.Exec(`INSERT INTO projects(id, workspace_id, name, path) VALUES(1, 1, 'p', '/tmp/p')`)
	ss := &SessionStore{Store: store}

	// Clean exit.
	sClean, _ := ss.Create(1, "opencode", "default", 100, 1)
	if err := ss.MarkExited(sClean.ID, 0); err != nil {
		t.Fatal(err)
	}
	got, _ := ss.Get(sClean.ID)
	if got.Status != "exited" {
		t.Errorf("clean exit: status %q want exited", got.Status)
	}

	// Failed (non-zero exit code).
	sFail, _ := ss.Create(1, "opencode", "default", 101, 2)
	ss.MarkExited(sFail.ID, 2)
	got, _ = ss.Get(sFail.ID)
	if got.Status != "failed" {
		t.Errorf("non-zero exit: status %q want failed", got.Status)
	}
	var exitCode int
	store.db.QueryRow(`SELECT exit_code FROM sessions WHERE id=?`, sFail.ID).Scan(&exitCode)
	if exitCode != 2 {
		t.Errorf("exit_code: got %d want 2", exitCode)
	}
}
