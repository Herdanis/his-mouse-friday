package daemon

import (
	"testing"
)

func TestSessionStore_CreateAndGet(t *testing.T) {
	r := &Registry{Store: newTestStore(t)}
	r.AddWorkspace("companyA")
	r.AddProject("companyA", "payment-service", "/tmp/payment")

	ss := &SessionStore{Store: r.Store}
	s, err := ss.Create(1, "opencode", "default", 12345)
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
	ss := &SessionStore{Store: newTestStore(t)}
	s, _ := ss.Create(1, "opencode", "default", 99)
	if err := ss.SetStatus(s.ID, "failed"); err != nil {
		t.Fatal(err)
	}
	got, _ := ss.Get(s.ID)
	if got.Status != "failed" {
		t.Errorf("status: got %q want failed", got.Status)
	}
}
