package tui

import (
	"encoding/json"

	tea "github.com/charmbracelet/bubbletea"
)

// ============================================
// Threads panel — stub, filled by the threads task
// ============================================

type threadsModel struct {
	fetch func() (json.RawMessage, error)
}

func newThreadsModel(f fetchers) threadsModel { return threadsModel{fetch: f.threadList} }

func (t threadsModel) refresh() tea.Cmd { return fetchCmd(t.fetch) }
func (t threadsModel) update(tea.Msg) (threadsModel, tea.Cmd) {
	return t, nil
}
func (t threadsModel) view(w, h int) string { return notImpl("threads") }
