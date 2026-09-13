package tui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/herdanis/his-mouse-friday/internal/daemon"
)

func writeAgentLog(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "hmf.log")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	return p
}

func agentsFixture(logPath string) agentsModel {
	return newAgentsModel(stubFetcher(map[string]json.RawMessage{
		"session_list": mustJSON([]daemon.SessionListItem{
			{ID: 7, Name: "ab000-child", Project: "child", Status: "active"},
		}),
	}), logPath)
}

func TestAgentRowCollapsedFormat(t *testing.T) {
	am := newAgentsModel(stubFetcher(map[string]json.RawMessage{
		"session_list": mustJSON([]daemon.SessionListItem{{
			ID: 7, Name: "ab000-child", Project: "child", Status: "active",
			ProgressNote: "80% done eta_minutes: 5", ETAMinutes: 5,
		}}),
	}), filepath.Join(t.TempDir(), "hmf.log"))
	am.refreshNow()
	v := am.view(120, 24)
	for _, want := range []string{"ab000-child", "working", "80% done", "5m"} {
		if !strings.Contains(v, want) {
			t.Fatalf("row missing %q:\n%s", want, v)
		}
	}
}

func TestAgentRowStaleETA(t *testing.T) {
	am := newAgentsModel(stubFetcher(map[string]json.RawMessage{
		"session_list": mustJSON([]daemon.SessionListItem{{
			ID: 7, Name: "ab000-child", Project: "child", Status: "active",
			ProgressNote: "80% done", ETAMinutes: 5, ProgressAgeSecs: 3600,
		}}),
	}), filepath.Join(t.TempDir(), "hmf.log"))
	am.refreshNow()
	v := am.view(120, 24)
	if !strings.Contains(v, "5m (stale)") {
		t.Fatalf("stale ETA must be marked:\n%s", v)
	}
}

func TestAgentsEnterExpandsTail(t *testing.T) {
	p := writeAgentLog(t,
		"[agent#7 ab000-child] reading files",
		"[agent#7 ab000-child] ERROR: tests failed",
	)
	am := agentsFixture(p)
	am.refreshNow()
	am, _ = am.update(tea.KeyMsg{Type: tea.KeyEnter})
	if am.expanded != 0 {
		t.Fatalf("enter must expand selected row, got %d", am.expanded)
	}
	v := am.view(120, 24)
	if !strings.Contains(v, "reading files") || !strings.Contains(v, "ERROR: tests failed") {
		t.Fatalf("expanded view must show tail:\n%s", v)
	}
}

func TestAgentsErrorFilter(t *testing.T) {
	p := writeAgentLog(t,
		"[agent#7 ab000-child] reading files",
		"[agent#7 ab000-child] ERROR: tests failed",
	)
	am := agentsFixture(p)
	am.refreshNow()
	am, _ = am.update(tea.KeyMsg{Type: tea.KeyEnter})
	am, _ = am.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("e")})
	if !am.errorsOnly {
		t.Fatal("e must arm ERROR filter in expanded mode")
	}
	v := am.view(120, 24)
	if strings.Contains(v, "reading files") {
		t.Fatalf("filtered view must hide non-error lines:\n%s", v)
	}
	if !strings.Contains(v, "ERROR: tests failed") {
		t.Fatalf("filtered view must keep errors:\n%s", v)
	}
}

func TestAgentsEscCollapses(t *testing.T) {
	p := writeAgentLog(t, "[agent#7 ab000-child] hi")
	am := agentsFixture(p)
	am.refreshNow()
	am, _ = am.update(tea.KeyMsg{Type: tea.KeyEnter})
	am, _ = am.update(tea.KeyMsg{Type: tea.KeyEsc})
	if am.expanded != -1 {
		t.Fatalf("esc must collapse, got %d", am.expanded)
	}
}

func TestAgentsTailRefreshesOnTick(t *testing.T) {
	p := writeAgentLog(t, "[agent#7 ab000-child] old line")
	am := agentsFixture(p)
	am.refreshNow()
	am, _ = am.update(tea.KeyMsg{Type: tea.KeyEnter})
	if err := os.WriteFile(p, []byte("[agent#7 ab000-child] old line\n[agent#7 ab000-child] fresh line\n"), 0644); err != nil {
		t.Fatal(err)
	}
	am, _ = am.update(agentsMsg{rows: am.rows})
	if !strings.Contains(am.view(120, 24), "fresh line") {
		t.Fatal("tick refresh must re-tail the expanded row")
	}
}

func TestAgentsDownOnEmptyRows(t *testing.T) {
	am := newAgentsModel(stubFetcher(map[string]json.RawMessage{}), filepath.Join(t.TempDir(), "hmf.log"))
	am.refreshNow()
	am, _ = am.update(tea.KeyMsg{Type: tea.KeyDown})
	if am.sel != 0 {
		t.Fatalf("down on empty rows must not move sel, got %d", am.sel)
	}
}

func TestAgentsExpandedShowsRerr(t *testing.T) {
	p := writeAgentLog(t, "[agent#7 ab000-child] hi")
	am := agentsFixture(p)
	am.refreshNow()
	am, _ = am.update(tea.KeyMsg{Type: tea.KeyEnter})
	am.rerr = "read failed"
	if !strings.Contains(am.view(120, 24), "read failed") {
		t.Fatal("expanded view must surface rerr")
	}
}

func TestAgentsRerrClearsOnRefresh(t *testing.T) {
	am := agentsFixture(filepath.Join(t.TempDir(), "hmf.log"))
	am.rerr = "boom"
	am.refreshNow()
	if am.rerr != "" {
		t.Fatalf("successful refresh must clear rerr, got %q", am.rerr)
	}
}

func TestAgentsNavAndGuard(t *testing.T) {
	am := newAgentsModel(stubFetcher(map[string]json.RawMessage{
		"session_list": mustJSON([]daemon.SessionListItem{
			{ID: 1, Name: "a", Status: "active"},
			{ID: 2, Name: "b", Status: "exited"},
		}),
	}), filepath.Join(t.TempDir(), "hmf.log"))
	am.refreshNow()
	am, _ = am.update(tea.KeyMsg{Type: tea.KeyDown})
	if am.sel != 1 {
		t.Fatal("down must move selection")
	}
	am, _ = am.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if am.sel != 1 {
		t.Fatal("j at bottom row must not move")
	}
	am, _ = am.update(tea.KeyMsg{Type: tea.KeyUp})
	if am.sel != 0 {
		t.Fatal("up must move selection back")
	}
	am, _ = am.update(agentsMsg{rows: []daemon.SessionListItem{{ID: 1, Name: "a", Status: "active"}}})
	if am.sel != 0 {
		t.Fatalf("sel must clamp when rows shrink, got %d", am.sel)
	}
}
