package tui

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/herdanis/his-mouse-friday/internal/daemon"
)

// ============================================
// Projects panel
// ============================================

// projectsMsg carries a refreshed project_list payload to the panel.
type projectsMsg struct{ rows []daemon.ProjectListItem }

type projectsModel struct {
	list func() (json.RawMessage, error)
	add  func(json.RawMessage) (json.RawMessage, error)
	del  func(json.RawMessage) (json.RawMessage, error)

	rows    []daemon.ProjectListItem
	sel     int
	rerr    string
	confirm bool

	adding bool
	// inputs[0] collects "workspace/name" (split on the slash on submit),
	// inputs[1] the path — the brief specifies a two-field form.
	inputs [2]textinput.Model
	focus  int
}

func newProjectsModel(f fetchers) projectsModel {
	wsName := textinput.New()
	wsName.Placeholder = "workspace/name"
	wsName.CharLimit = 120
	path := textinput.New()
	path.Placeholder = "path"
	path.CharLimit = 200
	return projectsModel{
		list: f.projectList, add: f.projectAdd, del: f.projectDel,
		inputs: [2]textinput.Model{wsName, path},
	}
}

// refreshNow does a synchronous project_list fetch — tests and post-CRUD reload.
func (p *projectsModel) refreshNow() {
	raw, err := p.list()
	if err != nil {
		p.rerr = err.Error()
		return
	}
	var rows []daemon.ProjectListItem
	if err := json.Unmarshal(raw, &rows); err != nil {
		p.rerr = err.Error()
		return
	}
	p.rows = rows
	p.sel = min(p.sel, max(0, len(rows)-1))
	p.rerr = ""
}

func (p projectsModel) refresh() tea.Cmd {
	return func() tea.Msg {
		raw, err := p.list()
		if err != nil {
			return fetchMsg{err: err}
		}
		var rows []daemon.ProjectListItem
		if err := json.Unmarshal(raw, &rows); err != nil {
			return fetchMsg{err: err}
		}
		return projectsMsg{rows: rows}
	}
}

func (p projectsModel) update(msg tea.Msg) (projectsModel, tea.Cmd) {
	switch msg := msg.(type) {
	case projectsMsg:
		p.rows = msg.rows
		p.sel = min(p.sel, max(0, len(p.rows)-1))
		p.rerr = ""
		return p, nil
	case tea.KeyMsg:
		return p.keyUpdate(msg)
	}
	return p, nil
}

func (p projectsModel) capturing() bool { return p.adding || p.confirm }

func (p projectsModel) keyUpdate(msg tea.KeyMsg) (projectsModel, tea.Cmd) {
	// Confirm consumes the y/n first — never leaks into other branches.
	if p.confirm {
		p.confirm = false
		if msg.String() != "y" || len(p.rows) == 0 {
			return p, nil
		}
		r := p.rows[p.sel]
		params, _ := json.Marshal(map[string]string{"workspace": r.Workspace, "name": r.Name})
		if _, err := p.del(params); err != nil {
			p.rerr = err.Error()
			return p, nil
		}
		p.refreshNow()
		return p, nil
	}
	if p.adding {
		switch msg.String() {
		case "esc":
			p.adding = false
			return p, nil
		case "tab":
			p.focus = (p.focus + 1) % len(p.inputs)
			for i := range p.inputs {
				p.inputs[i].Blur()
			}
			p.inputs[p.focus].Focus()
			return p, nil
		case "enter":
			return p.submit()
		}
		var cmd tea.Cmd
		p.inputs[p.focus], cmd = p.inputs[p.focus].Update(msg)
		return p, cmd
	}
	switch msg.String() {
	case "up", "k":
		p.sel = max(0, p.sel-1)
	case "down", "j":
		p.sel = min(len(p.rows)-1, p.sel+1)
	case "a":
		p.adding = true
		p.focus = 0
		for i := range p.inputs {
			p.inputs[i].SetValue("")
		}
		p.inputs[0].Focus()
		p.inputs[1].Blur()
	case "d":
		if len(p.rows) > 0 {
			p.confirm = true
		}
	}
	return p, nil
}

// submit parses "workspace/name" and fires project_add synchronously — the
// RPC has a short timeout and there is nothing else to draw meanwhile.
func (p projectsModel) submit() (projectsModel, tea.Cmd) {
	ws, name, ok := strings.Cut(strings.TrimSpace(p.inputs[0].Value()), "/")
	ws, name = strings.TrimSpace(ws), strings.TrimSpace(name)
	path := strings.TrimSpace(p.inputs[1].Value())
	if !ok || ws == "" || name == "" || path == "" {
		p.rerr = "form needs workspace/name and path"
		return p, nil
	}
	params, _ := json.Marshal(map[string]string{"workspace": ws, "name": name, "path": path})
	if _, err := p.add(params); err != nil {
		p.rerr = err.Error()
		return p, nil
	}
	p.adding = false
	p.refreshNow()
	return p, nil
}

func (p projectsModel) view(w, h int) string {
	if p.adding {
		return p.formView()
	}
	if p.rerr != "" {
		return "\n  " + styFailed.Render(p.rerr)
	}
	if len(p.rows) == 0 {
		return "\n  " + styDim.Render("no projects — a to add")
	}
	var b strings.Builder
	for i, r := range p.rows {
		marker := "  "
		if i == p.sel {
			marker = "> "
		}
		b.WriteString(marker + fmt.Sprintf("%s/%s %s", r.Workspace, r.Name, r.Path) + "\n")
	}
	if p.confirm && len(p.rows) > 0 {
		r := p.rows[p.sel]
		b.WriteString(" " + styFailed.Render(fmt.Sprintf("delete %s/%s? y/n", r.Workspace, r.Name)))
	}
	return b.String()
}

func (p projectsModel) formView() string {
	var b strings.Builder
	b.WriteString(" add project\n")
	for i, in := range p.inputs {
		marker := "  "
		if i == p.focus {
			marker = "> "
		}
		b.WriteString(marker + in.View() + "\n")
	}
	b.WriteString(" " + styDim.Render("tab field · enter save · esc cancel"))
	if p.rerr != "" {
		b.WriteString("  " + styFailed.Render(p.rerr))
	}
	return b.String()
}
