package tui

import (
	"encoding/json"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/herdanis/his-mouse-friday/internal/daemon"
	"github.com/herdanis/his-mouse-friday/internal/protocol"
)

// ============================================
// hmf orchestrator TUI
// ============================================

const tickEvery = 2 * time.Second

// Deliberate duplication of internal/cli/monitor.go's palette: monitor goes
// away once the TUI replaces it, and sharing a file across packages that
// delete at different times buys nothing.
var (
	cAccent  = lipgloss.AdaptiveColor{Light: "#0969da", Dark: "#58a6ff"}
	cSuccess = lipgloss.AdaptiveColor{Light: "#1a7f37", Dark: "#3fb950"}
	cDanger  = lipgloss.AdaptiveColor{Light: "#cf222e", Dark: "#f85149"}
	cDoneFg  = lipgloss.AdaptiveColor{Light: "#8250df", Dark: "#a371f7"}
	cMuted   = lipgloss.AdaptiveColor{Light: "#59636e", Dark: "#8b949e"}
	cText    = lipgloss.AdaptiveColor{Light: "#1f2328", Dark: "#e6edf3"}
)

var (
	styTitle   = lipgloss.NewStyle().Bold(true).Foreground(cText)
	styText    = lipgloss.NewStyle().Foreground(cText)
	styDim     = lipgloss.NewStyle().Foreground(cMuted)
	styWorking = lipgloss.NewStyle().Foreground(cSuccess)
	styFailed  = lipgloss.NewStyle().Foreground(cDanger)
	styAttn    = lipgloss.NewStyle().Foreground(cDanger)
	styDone    = lipgloss.NewStyle().Foreground(cDoneFg)
	styKey     = lipgloss.NewStyle().Foreground(cAccent)
)

type panel int

const (
	panelThreads panel = iota
	panelAgents
	panelProjects
	panelTodos
)

var panelNames = [...]string{"threads", "agents", "projects", "todos"}

type tickMsg time.Time

// fetchMsg carries a panel's refresh result. Errors land on the model so the
// title bar shows daemon state; data handling lives in each panel.
type fetchMsg struct{ err error }

// fetchers injects the socket calls so unit tests never touch a socket.
type fetchers struct {
	threadList   func() (json.RawMessage, error)
	sessionList  func() (json.RawMessage, error)
	projectList  func() (json.RawMessage, error)
	todoThreads  func() (json.RawMessage, error)
	readThread   func(json.RawMessage) (json.RawMessage, error)
	threadDelete func(json.RawMessage) (json.RawMessage, error)
	projectAdd   func(json.RawMessage) (json.RawMessage, error)
	projectDel   func(json.RawMessage) (json.RawMessage, error)
	todoList     func(json.RawMessage) (json.RawMessage, error)
	todoAdd      func(json.RawMessage) (json.RawMessage, error)
	todoUpdate   func(json.RawMessage) (json.RawMessage, error)
	todoDelete   func(json.RawMessage) (json.RawMessage, error)
}

func defaultFetchers() fetchers {
	c := func(method string) func() (json.RawMessage, error) {
		return func() (json.RawMessage, error) { return protocol.Call(method, struct{}{}) }
	}
	p := func(method string) func(json.RawMessage) (json.RawMessage, error) {
		return func(params json.RawMessage) (json.RawMessage, error) { return protocol.Call(method, params) }
	}
	return fetchers{
		threadList: c("thread_list"), sessionList: c("session_list"),
		projectList: c("project_list"), todoThreads: c("todo_threads"),
		readThread: p("read_thread"), threadDelete: p("thread_delete"),
		projectAdd: p("project_add"), projectDel: p("project_delete"),
		todoList: p("todo_list"), todoAdd: p("todo_add"),
		todoUpdate: p("todo_update"), todoDelete: p("todo_delete"),
	}
}

func fetchCmd(f func() (json.RawMessage, error)) tea.Cmd {
	return func() tea.Msg {
		_, err := f()
		return fetchMsg{err: err}
	}
}

// ============================================
// Model
// ============================================

type model struct {
	panel    panel
	w, h     int
	err      error
	fetchers fetchers
	threads  threadsModel
	agents   agentsModel
	projects projectsModel
	todos    todosModel
}

func initialModel() model {
	f := defaultFetchers()
	return model{
		panel:    panelThreads,
		fetchers: f,
		threads:  newThreadsModel(f),
		agents:   newAgentsModel(f, daemon.LogPath()),
		projects: newProjectsModel(f),
		todos:    newTodosModel(f),
	}
}

func Run() error {
	if err := daemon.EnsureRunning(); err != nil {
		return err
	}
	_, err := tea.NewProgram(initialModel(),
		tea.WithAltScreen(), tea.WithMouseCellMotion()).Run()
	return err
}

func (m model) Init() tea.Cmd {
	return tea.Batch(tickCmd(), m.refreshActive())
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	mm, cmd := m.update(msg)
	return mm, cmd
}

func (m model) update(msg tea.Msg) (model, tea.Cmd) {
	var cmd tea.Cmd
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height

	case tea.KeyMsg:
		// While the active panel holds an open form/input/confirm it claims
		// all keys (q, digits, tab included) — ctrl+c always quits.
		if m.activePanelCapturing() && msg.String() != "ctrl+c" {
			switch m.panel {
			case panelThreads:
				m.threads, cmd = m.threads.update(msg)
			case panelAgents:
				m.agents, cmd = m.agents.update(msg)
			case panelProjects:
				m.projects, cmd = m.projects.update(msg)
			case panelTodos:
				m.todos, cmd = m.todos.update(msg)
			}
			return m, cmd
		}
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "1", "2", "3", "4":
			m.panel = panel(msg.String()[0] - '1')
			return m, m.refreshActive()
		case "tab":
			m.panel = (m.panel + 1) % panel(len(panelNames))
			return m, m.refreshActive()
		}
		// Keys not consumed by the chrome go to the active panel.
		switch m.panel {
		case panelThreads:
			m.threads, cmd = m.threads.update(msg)
		case panelAgents:
			m.agents, cmd = m.agents.update(msg)
		case panelProjects:
			m.projects, cmd = m.projects.update(msg)
		case panelTodos:
			m.todos, cmd = m.todos.update(msg)
		}

	case tickMsg:
		// Only the active panel refreshes — one socket round-trip set per
		// tick, not four.
		return m, tea.Batch(tickCmd(), m.refreshActive())

	case fetchMsg:
		m.err = msg.err

	case threadsMsg:
		m.threads, cmd = m.threads.update(msg)
	case agentsMsg:
		m.agents, cmd = m.agents.update(msg)
	case projectsMsg:
		m.projects, cmd = m.projects.update(msg)
	case todosMsg:
		m.todos, cmd = m.todos.update(msg)
	}
	return m, cmd
}

// activePanelCapturing reports whether the active panel holds an open
// form/input/confirm and should claim keys before the chrome does.
func (m model) activePanelCapturing() bool {
	switch m.panel {
	case panelThreads:
		return m.threads.capturing()
	case panelAgents:
		return m.agents.capturing()
	case panelProjects:
		return m.projects.capturing()
	case panelTodos:
		return m.todos.capturing()
	}
	return false
}

func (m model) refreshActive() tea.Cmd {
	switch m.panel {
	case panelThreads:
		return m.threads.refresh()
	case panelAgents:
		return m.agents.refresh()
	case panelProjects:
		return m.projects.refresh()
	case panelTodos:
		return m.todos.refresh()
	}
	return nil
}
func tickCmd() tea.Cmd {
	return tea.Tick(tickEvery, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func notImpl(name string) string {
	return "\n  " + styDim.Render(name + ": not implemented yet")
}

// ============================================
// View
// ============================================

func (m model) View() string {
	return m.titleBar() + "\n" + m.body() + "\n" + m.footer()
}

func (m model) titleBar() string {
	state := styWorking.Render("connected")
	if m.err != nil {
		state = styFailed.Render("daemon unreachable")
	}
	return " " + styTitle.Render("hmf") + styDim.Render(" — ") +
		styText.Render(panelNames[m.panel]) + styDim.Render(" — ") + state
}

func (m model) body() string {
	h := max(1, m.h-2)
	w := max(1, m.w)
	switch m.panel {
	case panelThreads:
		return m.threads.view(w, h)
	case panelAgents:
		return m.agents.view(w, h)
	case panelProjects:
		return m.projects.view(w, h)
	case panelTodos:
		return m.todos.view(w, h)
	}
	return ""
}

func (m model) footer() string {
	return " " + styKey.Render("1-4") + styDim.Render(" panel · ") +
		styKey.Render("tab") + styDim.Render(" next · ") +
		styKey.Render("q") + styDim.Render(" quit")
}
