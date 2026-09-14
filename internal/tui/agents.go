package tui

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/herdanis/his-mouse-friday/internal/daemon"
)

// ============================================
// Agents panel
// ============================================

// agentsMsg carries a refreshed session_list payload to the panel.
type agentsMsg struct{ rows []daemon.SessionListItem }

type agentsModel struct {
	fetch   func() (json.RawMessage, error)
	logPath string

	rows []daemon.SessionListItem
	sel  int
	rerr string

	// expanded is the row index whose log detail is open, -1 = none.
	expanded   int
	errorsOnly bool
	tail       []string
	vp         viewport.Model

	// lastLog caches each session's newest log line for the collapsed row —
	// the view renders on every keypress and re-tailing per frame would scan
	// the log file far more than once per refresh tick.
	lastLog map[int64]string
}

func newAgentsModel(f fetchers, logPath string) agentsModel {
	return agentsModel{
		fetch: f.sessionList, logPath: logPath,
		expanded: -1, lastLog: map[int64]string{},
	}
}

// refreshNow does a synchronous session_list fetch — tests and first render.
func (a *agentsModel) refreshNow() {
	raw, err := a.fetch()
	if err != nil {
		a.rerr = err.Error()
		return
	}
	var rows []daemon.SessionListItem
	if err := json.Unmarshal(raw, &rows); err != nil {
		a.rerr = err.Error()
		return
	}
	a.setRows(rows)
}

func (a *agentsModel) setRows(rows []daemon.SessionListItem) {
	a.rows = rows
	a.sel = min(a.sel, max(0, len(rows)-1))
	if a.expanded >= len(rows) {
		a.expanded = -1
		a.tail = nil
	}
	a.refreshLastLog()
	a.refreshTail()
	a.rerr = ""
}

// ponytail: full backwards scan per session for a deep last-line; per-session
// tail-offset bookmarks if session count × log size ever hurts.
func (a *agentsModel) refreshLastLog() {
	for _, r := range a.rows {
		lines, err := tailLog(a.logPath, fmt.Sprintf("agent#%d ", r.ID), 1, false)
		if err == nil && len(lines) > 0 {
			a.lastLog[r.ID] = firstLine(lines[len(lines)-1], 40)
		}
	}
}

func (a *agentsModel) refreshTail() {
	if a.expanded < 0 || a.expanded >= len(a.rows) {
		return
	}
	id := a.rows[a.expanded].ID
	lines, err := tailLog(a.logPath, fmt.Sprintf("agent#%d ", id), 500, a.errorsOnly)
	if err != nil {
		a.rerr = err.Error()
		return
	}
	a.tail = lines
}

func (a agentsModel) refresh() tea.Cmd {
	return func() tea.Msg {
		raw, err := a.fetch()
		if err != nil {
			return fetchMsg{err: err}
		}
		var rows []daemon.SessionListItem
		if err := json.Unmarshal(raw, &rows); err != nil {
			return fetchMsg{err: err}
		}
		return agentsMsg{rows: rows}
	}
}

func (a agentsModel) update(msg tea.Msg) (agentsModel, tea.Cmd) {
	switch msg := msg.(type) {
	case agentsMsg:
		a.setRows(msg.rows)
		return a, nil
	case tea.KeyMsg:
		return a.keyUpdate(msg)
	}
	return a, nil
}

func (a agentsModel) capturing() bool { return false }

func (a agentsModel) keyUpdate(msg tea.KeyMsg) (agentsModel, tea.Cmd) {
	if a.expanded >= 0 {
		switch msg.String() {
		case "esc":
			a.expanded = -1
			a.tail = nil
			return a, nil
		case "e":
			a.errorsOnly = !a.errorsOnly
			a.refreshTail()
			return a, nil
		}
		var cmd tea.Cmd
		a.vp, cmd = a.vp.Update(msg)
		return a, cmd
	}
	switch msg.String() {
	case "up", "k":
		a.sel = max(0, a.sel-1)
	case "down", "j":
		if len(a.rows) == 0 {
			return a, nil
		}
		a.sel = min(len(a.rows)-1, a.sel+1)
	case "enter":
		if len(a.rows) == 0 {
			return a, nil
		}
		a.expanded = a.sel
		a.errorsOnly = false
		a.vp.GotoTop()
		a.refreshTail()
	}
	return a, nil
}

// agentStatusText maps the session row status to the harness vocabulary.
func agentStatusText(s string) string {
	if s == "active" {
		return "working"
	}
	return s
}

func agentDotStyle(s string) lipgloss.Style {
	switch agentStatusText(s) {
	case "working":
		return styWorking
	case "failed":
		return styFailed
	case "exited":
		return styDone
	default:
		return styDim
	}
}

func agentRow(lastLog map[int64]string, r daemon.SessionListItem) string {
	detail := r.ProgressNote
	if detail == "" {
		detail = r.CurrentStep
	}
	if detail == "" {
		detail = "-"
	}
	eta := "-"
	if r.ETAMinutes > 0 {
		eta = fmt.Sprintf("%dm", r.ETAMinutes)
		if r.ProgressAgeSecs > 15*60 {
			eta += " (stale)"
		}
	}
	return fmt.Sprintf("%s %s  %s  %s  %s  %s  %s",
		agentDotStyle(r.Status).Render("●"), r.Name, r.Project,
		agentStatusText(r.Status), detail, eta, lastLog[r.ID])
}

func (a agentsModel) view(w, h int) string {
	if a.expanded >= 0 && a.expanded < len(a.rows) {
		return a.expandedView(w, h)
	}
	if a.rerr != "" {
		return "\n  " + styFailed.Render(a.rerr)
	}
	if len(a.rows) == 0 {
		return "\n  " + styDim.Render("no agents")
	}
	var b strings.Builder
	for i, r := range a.rows {
		marker := "  "
		if i == a.sel {
			marker = "> "
		}
		b.WriteString(marker + agentRow(a.lastLog, r) + "\n")
	}
	return b.String()
}

func (a agentsModel) expandedView(w, h int) string {
	vp := a.vp
	vp.Width, vp.Height = w, max(1, h-1)
	vp.SetContent(strings.Join(a.tail, "\n"))
	head := ""
	if a.rerr != "" {
		head = styFailed.Render(a.rerr) + "\n"
	}
	filter := ""
	if a.errorsOnly {
		filter = "  " + styFailed.Render("ERROR filter")
	}
	return head + vp.View() + "\n" + styDim.Render("esc back · e error filter · j/k scroll") + filter
}
