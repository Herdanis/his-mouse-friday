package tui

import (
	"encoding/json"
	"errors"
	"testing"

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
	}
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
