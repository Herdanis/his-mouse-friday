package tui

import (
	"encoding/json"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/herdanis/his-mouse-friday/internal/daemon"
)

func TestProjectsRenderRow(t *testing.T) {
	pm := newProjectsModel(stubFetcher(map[string]json.RawMessage{
		"project_list": mustJSON([]daemon.ProjectListItem{
			{Name: "payment-service", Path: "/code/payment"},
		}),
	}))
	pm.refreshNow()
	v := pm.view(80, 24)
	if !strings.Contains(v, "companyA/payment-service") || !strings.Contains(v, "/code/payment") {
		t.Fatalf("row must show workspace/name and path:\n%s", v)
	}
}

func TestProjectsAddFlow(t *testing.T) {
	var calls []recordedCall
	pm := newProjectsModel(recordingFetcher(&calls, map[string]json.RawMessage{
		"project_list": mustJSON([]daemon.ProjectListItem{}),
	}))
	pm.refreshNow()
	pm, _ = pm.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	if !pm.adding {
		t.Fatal("a must open the add form")
	}
	pm, _ = pm.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("companyA/pay")})
	pm, _ = pm.update(tea.KeyMsg{Type: tea.KeyTab})
	pm, _ = pm.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/code/pay")})
	pm, _ = pm.update(tea.KeyMsg{Type: tea.KeyEnter})
	if pm.adding {
		t.Fatal("enter must submit and close the form")
	}
	c, ok := lastCall(calls, "project_add")
	if !ok {
		t.Fatalf("submit must call project_add, got %+v", calls)
	}
	var p map[string]string
	if err := json.Unmarshal(c.Params, &p); err != nil {
		t.Fatalf("bad project_add params: %v", err)
	}
	if p["workspace"] != "companyA" || p["name"] != "pay" || p["path"] != "/code/pay" {
		t.Fatalf("project_add params wrong: %+v", p)
	}
}

func TestProjectsDeleteConfirm(t *testing.T) {
	var calls []recordedCall
	pm := newProjectsModel(recordingFetcher(&calls, map[string]json.RawMessage{
		"project_list": mustJSON([]daemon.ProjectListItem{
			{Name: "svc", Path: "/code/svc"},
		}),
	}))
	pm.refreshNow()
	pm, _ = pm.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	if !pm.confirm {
		t.Fatal("d must arm confirm")
	}
	pm, _ = pm.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	if pm.confirm {
		t.Fatal("n must cancel without deleting")
	}
	if _, ok := lastCall(calls, "project_delete"); ok {
		t.Fatal("n must not delete")
	}
	pm, _ = pm.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	pm, _ = pm.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	c, ok := lastCall(calls, "project_delete")
	if !ok {
		t.Fatalf("y must call project_delete, got %+v", calls)
	}
	var p map[string]string
	if err := json.Unmarshal(c.Params, &p); err != nil {
		t.Fatalf("bad project_delete params: %v", err)
	}
	if p["workspace"] != "co" || p["name"] != "svc" {
		t.Fatalf("project_delete params wrong: %+v", p)
	}
}

func TestProjectsEmptyRowsGuard(t *testing.T) {
	var calls []recordedCall
	pm := newProjectsModel(recordingFetcher(&calls, map[string]json.RawMessage{}))
	pm.refreshNow()
	pm, _ = pm.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	pm, _ = pm.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	pm, _ = pm.update(tea.KeyMsg{Type: tea.KeyDown})
	pm.view(80, 24)
	if _, ok := lastCall(calls, "project_add"); ok {
		t.Fatalf("no write RPC may fire on empty list, got %+v", calls)
	}
	if _, ok := lastCall(calls, "project_delete"); ok {
		t.Fatalf("no write RPC may fire on empty list, got %+v", calls)
	}
}

func TestProjectsFetchErrSurfacesAndClears(t *testing.T) {
	pm := newProjectsModel(stubFetcher(map[string]json.RawMessage{}))
	pm.refreshNow()
	if pm.rerr == "" {
		t.Fatal("fetch failure must land in rerr")
	}
	pm2, _ := pm.update(projectsMsg{rows: []daemon.ProjectListItem{
		{Name: "svc", Path: "/p"},
	}})
	if pm2.rerr != "" {
		t.Fatalf("successful projectsMsg must clear rerr, got %q", pm2.rerr)
	}
}
