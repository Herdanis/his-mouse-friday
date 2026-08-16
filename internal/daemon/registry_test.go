package daemon

import (
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestRegistry_AddWorkspaceAndProject(t *testing.T) {
	r := &Registry{Store: newTestStore(t)}
	ws, err := r.AddWorkspace("companyA")
	if err != nil {
		t.Fatal(err)
	}
	if ws.Name != "companyA" {
		t.Errorf("name: got %q", ws.Name)
	}
	proj, err := r.AddProject("companyA", "payment-service", "/tmp/payment")
	if err != nil {
		t.Fatal(err)
	}
	if proj.Name != "payment-service" || proj.Path != "/tmp/payment" {
		t.Errorf("got %+v", proj)
	}
}

func TestRegistry_ResolveByPath(t *testing.T) {
	r := &Registry{Store: newTestStore(t)}
	r.AddWorkspace("companyA")
	r.AddProject("companyA", "payment-service", "/tmp/payment")

	proj, ws, err := r.ResolveByPath("/tmp/payment")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if proj.Name != "payment-service" || ws.Name != "companyA" {
		t.Errorf("got proj=%+v ws=%+v", proj, ws)
	}
}

func TestRegistry_ResolveByPath_NotFound(t *testing.T) {
	r := &Registry{Store: newTestStore(t)}
	_, _, err := r.ResolveByPath("/nonexistent")
	if err == nil {
		t.Fatal("expected error for unregistered path")
	}
}

func TestRegistry_ListProjects(t *testing.T) {
	r := &Registry{Store: newTestStore(t)}
	r.AddWorkspace("companyA")
	r.AddProject("companyA", "payment-service", "/tmp/payment")
	r.AddProject("companyA", "user-service", "/tmp/user")

	projs, err := r.ListProjects("companyA")
	if err != nil {
		t.Fatal(err)
	}
	if len(projs) != 2 {
		t.Errorf("got %d projects want 2", len(projs))
	}
}

func TestRegistry_NameCollisionAcrossWorkspaces(t *testing.T) {
	r := &Registry{Store: newTestStore(t)}
	r.AddWorkspace("companyA")
	r.AddWorkspace("personal")
	_, err := r.AddProject("companyA", "payment-service", "/tmp/payment")
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.AddProject("personal", "payment-service", "/tmp/payment2")
	if err != nil {
		t.Fatalf("same name in different workspace should succeed: %v", err)
	}
}
