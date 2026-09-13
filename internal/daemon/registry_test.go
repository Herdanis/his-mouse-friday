package daemon

import (
	"path/filepath"
	"strings"
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

func TestRegistry_AddAndListProjects(t *testing.T) {
	r := &Registry{Store: newTestStore(t)}
	proj, err := r.AddProject("payment-service", "/tmp/payment")
	if err != nil {
		t.Fatal(err)
	}
	if proj.Name != "payment-service" || proj.Path != "/tmp/payment" || proj.ID == 0 {
		t.Errorf("got %+v", proj)
	}
	if _, err := r.AddProject("user-service", "/tmp/user"); err != nil {
		t.Fatal(err)
	}
	projs, err := r.ListProjects()
	if err != nil {
		t.Fatal(err)
	}
	if len(projs) != 2 {
		t.Errorf("got %d projects want 2", len(projs))
	}
}

func TestRegistry_AddProject_DuplicateName(t *testing.T) {
	r := &Registry{Store: newTestStore(t)}
	if _, err := r.AddProject("payment-service", "/tmp/payment"); err != nil {
		t.Fatal(err)
	}
	_, err := r.AddProject("payment-service", "/tmp/payment2")
	if err == nil {
		t.Fatal("expected duplicate-name error")
	}
	if !strings.Contains(err.Error(), "payment-service") {
		t.Errorf("error must name the conflict, got: %v", err)
	}
}

func TestRegistry_AddProject_DuplicatePath(t *testing.T) {
	r := &Registry{Store: newTestStore(t)}
	if _, err := r.AddProject("payment", "/tmp/payment"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.AddProject("other", "/tmp/payment"); err == nil {
		t.Fatal("expected error: path already registered under a different project")
	}
}

func TestRegistry_FindAndResolveProject(t *testing.T) {
	r := &Registry{Store: newTestStore(t)}
	r.AddProject("payment-service", "/tmp/payment")
	if _, err := r.FindProject("payment-service"); err != nil {
		t.Fatalf("find: %v", err)
	}
	if _, err := r.FindProject("ghost"); err == nil {
		t.Fatal("expected not-found for unknown name")
	}
	if _, err := r.ResolveProject("payment-service"); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, err := r.ResolveProject("ghost"); err == nil {
		t.Fatal("expected not-found resolve")
	}
}

func TestRegistry_ResolveByPath(t *testing.T) {
	r := &Registry{Store: newTestStore(t)}
	r.AddProject("payment-service", "/tmp/payment")
	proj, err := r.ResolveByPath("/tmp/payment")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if proj.Name != "payment-service" {
		t.Errorf("got %+v", proj)
	}
	if _, err := r.ResolveByPath("/nonexistent"); err == nil {
		t.Fatal("expected error for unregistered path")
	}
}

// DeleteProject must remove the project's sessions before the project itself,
// or sessions.project_id would dangle past the FK.
func TestRegistry_DeleteProject_DeletesSessionsFirst(t *testing.T) {
	r := &Registry{Store: newTestStore(t)}
	proj, err := r.AddProject("payment-service", "/tmp/payment")
	if err != nil {
		t.Fatal(err)
	}
	ss := &SessionStore{Store: r.Store}
	if _, err := ss.Create(proj.ID, "opencode", "default", 0, 0, 0, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := r.DeleteProject("payment-service"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	var n int
	r.Store.db.QueryRow(`SELECT count(*) FROM sessions`).Scan(&n)
	if n != 0 {
		t.Errorf("%d sessions left after project delete, want 0", n)
	}
	if err := r.DeleteProject("payment-service"); err != ErrNotFound {
		t.Errorf("second delete = %v, want ErrNotFound", err)
	}
}
