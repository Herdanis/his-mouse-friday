package tui

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
)

func TestPanelSwitchingAndTick(t *testing.T) {
	m := initialModel()
	m, _ = m.update(tea.WindowSizeMsg{Width: 80, Height: 24})
	if m.panel != panelThreads {
		t.Fatal("default panel must be threads")
	}
	m2, cmd := m.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("2")})
	if m2.panel != panelAgents {
		t.Fatal("key 2 must switch to agents")
	}
	if cmd == nil {
		t.Fatal("panel switch must kick a refresh cmd")
	}
	m3, _ := m2.update(tickMsg{})
	if m3.err != nil {
		t.Fatalf("tick fetch errors must land in m.err, got %v", m3.err)
	}
}

func TestTabCyclesAndQuit(t *testing.T) {
	m := initialModel()
	m, _ = m.update(tea.KeyMsg{Type: tea.KeyTab})
	if m.panel != panelAgents {
		t.Fatal("tab from threads must land on agents")
	}
	if _, cmd := m.update(tea.KeyMsg{Type: tea.KeyCtrlC}); cmd == nil {
		t.Fatal("ctrl+c must quit")
	}
}

func TestFetchErrLandsInModelError(t *testing.T) {
	m := initialModel()
	m, _ = m.update(fetchMsg{err: errors.New("socket down")})
	if m.err == nil {
		t.Fatal("fetch error must land in m.err")
	}
	m, _ = m.update(fetchMsg{})
	if m.err != nil {
		t.Fatal("nil fetch error must clear m.err")
	}
}

func TestFormOpenClaimsKeys(t *testing.T) {
	key := func(s string) tea.KeyMsg {
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}

	// Form open + q: form receives it, no quit, panel unchanged.
	m := initialModel()
	m.fetchers = stubFetcher(nil)
	m.panel = panelProjects
	m, _ = m.update(key("a"))
	if !m.projects.adding {
		t.Fatal("a must open the add form")
	}
	m2, _ := m.update(key("q"))
	if m2.panel != panelProjects {
		t.Fatal("panel must stay projects while form open")
	}
	if got := m2.projects.inputs[0].Value(); got != "q" {
		t.Fatalf("q must land in form input, got %q", got)
	}

	// Form open + 1: digit lands in the input, no panel switch.
	m3, _ := m2.update(key("1"))
	if m3.panel != panelProjects {
		t.Fatal("panel must stay projects while form open")
	}
	if got := m3.projects.inputs[0].Value(); got != "q1" {
		t.Fatalf("1 must land in form input, got %q", got)
	}
}

func TestNoFormQQuits(t *testing.T) {
	m := initialModel()
	m.fetchers = stubFetcher(nil)
	m.panel = panelProjects
	if _, cmd := m.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")}); cmd == nil {
		t.Fatal("q with no form open must quit")
	}
}

func TestReaderViewEmptyRowsNoPanic(t *testing.T) {
	// Confirm armed mid-read, then rows emptied by an async refresh —
	// readerView must not index into a nil slice.
	tm := threadsModel{readerOpen: true, confirm: true, confirmReader: true, vp: viewport.New(80, 20)}
	if tm.readerView(80, 20) == "" {
		t.Fatal("reader view must render")
	}
}

func TestListViewEmptyRowsConfirmNoPanic(t *testing.T) {
	// Confirm armed, then an async refresh emptied the rows — list views
	// must render the confirm line only when a row exists.
	tm := threadsModel{confirm: true}
	if tm.view(80, 20) == "" {
		t.Fatal("threads list view must render")
	}
	pm := projectsModel{confirm: true}
	if pm.view(80, 20) == "" {
		t.Fatal("projects list view must render")
	}
}

// stubFetcher builds a fetchers set answering from a fixed map; later tasks'
// panel tests share it. Methods absent from the map return nil, nil.
func stubFetcher(m map[string]json.RawMessage) fetchers {
	f := func(method string) func() (json.RawMessage, error) {
		return func() (json.RawMessage, error) { return m[method], nil }
	}
	p := func(method string) func(json.RawMessage) (json.RawMessage, error) {
		return func(json.RawMessage) (json.RawMessage, error) { return m[method], nil }
	}
	return fetchers{
		threadList:   f("thread_list"),
		sessionList:  f("session_list"),
		projectList:  f("project_list"),
		todoThreads:  f("todo_threads"),
		readThread:   p("read_thread"),
		threadDelete: p("thread_delete"),
		projectAdd:   p("project_add"),
		projectDel:   p("project_delete"),
		todoList:     p("todo_list"),
		todoAdd:      p("todo_add"),
		todoUpdate:   p("todo_update"),
		todoDelete:   p("todo_delete"),
	}
}

// recordedCall is one RPC seen by recordingFetcher.
type recordedCall struct {
	Method string
	Params json.RawMessage
}

// recordingFetcher records every call's method+params and answers from a
// fixed map; methods absent from the map return nil, nil.
func recordingFetcher(calls *[]recordedCall, results map[string]json.RawMessage) fetchers {
	noarg := func(method string) func() (json.RawMessage, error) {
		return func() (json.RawMessage, error) {
			*calls = append(*calls, recordedCall{Method: method})
			return results[method], nil
		}
	}
	p := func(method string) func(json.RawMessage) (json.RawMessage, error) {
		return func(params json.RawMessage) (json.RawMessage, error) {
			*calls = append(*calls, recordedCall{Method: method, Params: params})
			return results[method], nil
		}
	}
	return fetchers{
		threadList:   noarg("thread_list"),
		sessionList:  noarg("session_list"),
		projectList:  noarg("project_list"),
		todoThreads:  noarg("todo_threads"),
		readThread:   p("read_thread"),
		threadDelete: p("thread_delete"),
		projectAdd:   p("project_add"),
		projectDel:   p("project_delete"),
		todoList:     p("todo_list"),
		todoAdd:      p("todo_add"),
		todoUpdate:   p("todo_update"),
		todoDelete:   p("todo_delete"),
	}
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// lastCall returns the most recent RPC of the given method — post-CRUD
// refreshes append their own list fetch, so the write RPC is never last.
func lastCall(calls []recordedCall, method string) (recordedCall, bool) {
	for i := len(calls) - 1; i >= 0; i-- {
		if calls[i].Method == method {
			return calls[i], true
		}
	}
	return recordedCall{}, false
}
