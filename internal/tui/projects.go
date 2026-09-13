package tui

import (
	"encoding/json"

	tea "github.com/charmbracelet/bubbletea"
)

// ============================================
// Projects panel — stub, filled by the projects task
// ============================================

type projectsModel struct {
	fetch func() (json.RawMessage, error)
}

func newProjectsModel(f fetchers) projectsModel { return projectsModel{fetch: f.projectList} }

func (p projectsModel) refresh() tea.Cmd { return fetchCmd(p.fetch) }
func (p projectsModel) update(tea.Msg) (projectsModel, tea.Cmd) {
	return p, nil
}
func (p projectsModel) view(w, h int) string { return notImpl("projects") }
