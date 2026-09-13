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
// Todos panel
// ============================================

// todoThreadRow mirrors the anonymous todo_threads result row in daemon.go.
type todoThreadRow struct {
	ThreadID int64  `json:"thread_id"`
	Preview  string `json:"preview"`
	Done     int    `json:"done"`
	Total    int    `json:"total"`
}

// todosMsg carries a refresh result; exactly one slice is set, matching the
// view the refresh was issued from (open thread → todos, else thread rows).
type todosMsg struct {
	threads []todoThreadRow
	todos   []daemon.Todo
}

type todosModel struct {
	threads func() (json.RawMessage, error)
	list    func(json.RawMessage) (json.RawMessage, error)
	add     func(json.RawMessage) (json.RawMessage, error)
	upd     func(json.RawMessage) (json.RawMessage, error)
	del     func(json.RawMessage) (json.RawMessage, error)

	threadRows []todoThreadRow
	threadSel  int

	// openThread is the thread whose todos are shown, 0 = thread list view.
	openThread int64
	todos      []daemon.Todo
	todoSel    int

	confirm bool
	rerr    string

	adding bool
	input  textinput.Model
}

func newTodosModel(f fetchers) todosModel {
	in := textinput.New()
	in.Placeholder = "new todo"
	in.CharLimit = 200
	return todosModel{
		threads: f.todoThreads, list: f.todoList, add: f.todoAdd,
		upd: f.todoUpdate, del: f.todoDelete,
		input: in,
	}
}

// refreshNow does a synchronous fetch for the current view — tests and
// post-CRUD reload.
func (t *todosModel) refreshNow() {
	if t.openThread != 0 {
		t.refreshTodosNow()
		return
	}
	raw, err := t.threads()
	if err != nil {
		t.rerr = err.Error()
		return
	}
	var rows []todoThreadRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.rerr = err.Error()
		return
	}
	t.threadRows = rows
	t.threadSel = min(t.threadSel, max(0, len(rows)-1))
	t.rerr = ""
}

func (t *todosModel) refreshTodosNow() {
	params, _ := json.Marshal(map[string]int64{"thread_id": t.openThread})
	raw, err := t.list(params)
	if err != nil {
		t.rerr = err.Error()
		return
	}
	var todos []daemon.Todo
	if err := json.Unmarshal(raw, &todos); err != nil {
		t.rerr = err.Error()
		return
	}
	t.todos = todos
	t.todoSel = min(t.todoSel, max(0, len(todos)-1))
	t.rerr = ""
}

func (t todosModel) refresh() tea.Cmd {
	if t.openThread != 0 {
		return func() tea.Msg {
			params, _ := json.Marshal(map[string]int64{"thread_id": t.openThread})
			raw, err := t.list(params)
			if err != nil {
				return fetchMsg{err: err}
			}
			var todos []daemon.Todo
			if err := json.Unmarshal(raw, &todos); err != nil {
				return fetchMsg{err: err}
			}
			return todosMsg{todos: todos}
		}
	}
	return func() tea.Msg {
		raw, err := t.threads()
		if err != nil {
			return fetchMsg{err: err}
		}
		var rows []todoThreadRow
		if err := json.Unmarshal(raw, &rows); err != nil {
			return fetchMsg{err: err}
		}
		return todosMsg{threads: rows}
	}
}

func (t todosModel) update(msg tea.Msg) (todosModel, tea.Cmd) {
	switch msg := msg.(type) {
	case todosMsg:
		if msg.threads != nil {
			t.threadRows = msg.threads
			t.threadSel = min(t.threadSel, max(0, len(t.threadRows)-1))
		}
		if msg.todos != nil {
			t.todos = msg.todos
			t.todoSel = min(t.todoSel, max(0, len(t.todos)-1))
		}
		t.rerr = ""
		return t, nil
	case tea.KeyMsg:
		return t.keyUpdate(msg)
	}
	return t, nil
}

func (t todosModel) keyUpdate(msg tea.KeyMsg) (todosModel, tea.Cmd) {
	// Confirm consumes the y/n first — never leaks into other branches.
	if t.confirm {
		t.confirm = false
		if msg.String() != "y" {
			return t, nil
		}
		if t.openThread == 0 || t.todoSel >= len(t.todos) {
			return t, nil
		}
		params, _ := json.Marshal(map[string]int64{"id": t.todos[t.todoSel].ID})
		if _, err := t.del(params); err != nil {
			t.rerr = err.Error()
			return t, nil
		}
		t.refreshNow()
		return t, nil
	}
	if t.adding {
		switch msg.String() {
		case "esc":
			t.adding = false
			return t, nil
		case "enter":
			return t.submit()
		}
		var cmd tea.Cmd
		t.input, cmd = t.input.Update(msg)
		return t, cmd
	}
	if t.openThread != 0 {
		return t.openKeyUpdate(msg)
	}
	switch msg.String() {
	case "up", "k":
		t.threadSel = max(0, t.threadSel-1)
	case "down", "j":
		t.threadSel = min(len(t.threadRows)-1, t.threadSel+1)
	case "enter":
		if len(t.threadRows) == 0 {
			return t, nil
		}
		t.openThread = t.threadRows[t.threadSel].ThreadID
		t.todoSel = 0
		t.refreshNow()
	}
	return t, nil
}

func (t todosModel) openKeyUpdate(msg tea.KeyMsg) (todosModel, tea.Cmd) {
	switch msg.String() {
	case "esc":
		t.openThread = 0
		t.todos = nil
		t.confirm = false
		t.refreshNow()
	case "up", "k":
		t.todoSel = max(0, t.todoSel-1)
	case "down", "j":
		t.todoSel = min(len(t.todos)-1, t.todoSel+1)
	case " ", "space":
		if t.todoSel >= len(t.todos) {
			return t, nil
		}
		// The daemon stores pending|done only — cycle between them.
		next := "done"
		if t.todos[t.todoSel].State == "done" {
			next = "pending"
		}
		params, _ := json.Marshal(map[string]any{"id": t.todos[t.todoSel].ID, "state": next})
		if _, err := t.upd(params); err != nil {
			t.rerr = err.Error()
			return t, nil
		}
		t.refreshNow()
	case "a":
		t.adding = true
		t.input.SetValue("")
		t.input.Focus()
	case "x":
		if len(t.todos) > 0 {
			t.confirm = true
		}
	}
	return t, nil
}

func (t todosModel) submit() (todosModel, tea.Cmd) {
	content := strings.TrimSpace(t.input.Value())
	if content == "" {
		t.rerr = "todo content is empty"
		return t, nil
	}
	params, _ := json.Marshal(map[string]any{"thread_id": t.openThread, "content": content})
	if _, err := t.add(params); err != nil {
		t.rerr = err.Error()
		return t, nil
	}
	t.adding = false
	t.refreshNow()
	return t, nil
}

// todoGlyph maps a todo state to its glyph. The daemon stores pending|done
// only; in-progress is reserved for a future daemon-side state.
func todoGlyph(s string) string {
	switch s {
	case "done":
		return "✓"
	case "in-progress":
		return "●"
	default:
		return "○"
	}
}

func (t todosModel) view(w, h int) string {
	if t.adding {
		return t.addingView()
	}
	if t.openThread != 0 {
		return t.openView()
	}
	if t.rerr != "" {
		return "\n  " + styFailed.Render(t.rerr)
	}
	if len(t.threadRows) == 0 {
		return "\n  " + styDim.Render("no threads with todos")
	}
	var b strings.Builder
	for i, r := range t.threadRows {
		marker := "  "
		if i == t.threadSel {
			marker = "> "
		}
		b.WriteString(marker + fmt.Sprintf("%d/%d done  %s", r.Done, r.Total, r.Preview) + "\n")
	}
	return b.String()
}

func (t todosModel) openView() string {
	if t.rerr != "" {
		return "\n  " + styFailed.Render(t.rerr)
	}
	if len(t.todos) == 0 {
		return "\n  " + styDim.Render("no todos — a to add")
	}
	var b strings.Builder
	for i, td := range t.todos {
		marker := "  "
		if i == t.todoSel {
			marker = "> "
		}
		style := styDim
		switch td.State {
		case "done":
			style = styDone
		case "in-progress":
			style = styWorking
		}
		b.WriteString(marker + style.Render(todoGlyph(td.State)+" "+td.Content) + "\n")
	}
	if t.confirm {
		b.WriteString(" " + styFailed.Render(fmt.Sprintf("delete todo #%d? y/n", t.todos[t.todoSel].ID)))
	}
	b.WriteString("\n " + styDim.Render("esc back · space cycle · a add · x delete"))
	return b.String()
}

func (t todosModel) addingView() string {
	return " add todo\n > " + t.input.View() + "\n " +
		styDim.Render("enter save · esc cancel")
}
