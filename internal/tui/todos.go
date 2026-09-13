package tui

import (
	"encoding/json"

	tea "github.com/charmbracelet/bubbletea"
)

// ============================================
// Todos panel — stub, filled by the todos task
// ============================================

type todosModel struct {
	fetch func() (json.RawMessage, error)
}

func newTodosModel(f fetchers) todosModel { return todosModel{fetch: f.todoThreads} }

func (t todosModel) refresh() tea.Cmd { return fetchCmd(t.fetch) }
func (t todosModel) update(tea.Msg) (todosModel, tea.Cmd) {
	return t, nil
}
func (t todosModel) view(w, h int) string { return notImpl("todos") }
