package tui

import (
	"encoding/json"

	tea "github.com/charmbracelet/bubbletea"
)

// ============================================
// Agents panel — stub, filled by the agents task
// ============================================

type agentsModel struct {
	fetch func() (json.RawMessage, error)
}

func newAgentsModel(f fetchers) agentsModel { return agentsModel{fetch: f.sessionList} }

func (a agentsModel) refresh() tea.Cmd { return fetchCmd(a.fetch) }
func (a agentsModel) update(tea.Msg) (agentsModel, tea.Cmd) {
	return a, nil
}
func (a agentsModel) view(w, h int) string { return notImpl("agents") }
