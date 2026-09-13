package tui

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/herdanis/his-mouse-friday/internal/daemon"
)

func TestThreadsRenderRow(t *testing.T) {
	tm := newThreadsModel(stubFetcher(map[string]json.RawMessage{
		"thread_list": mustJSON([]daemon.ThreadListItem{{
			ID: 500, Title: "do X please", From: "co/parent", To: "co/child",
			Status: "working", LastTS: time.Now().Format(time.RFC3339), MsgCount: 2,
		}}),
	}))
	tm.refreshNow()
	v := tm.view(80, 24)
	if !strings.Contains(v, "do X please") || !strings.Contains(v, "co/parent") {
		t.Fatalf("row missing fields:\n%s", v)
	}
	if !strings.Contains(v, "500") {
		t.Fatal("row missing thread id")
	}
}

func TestThreadsEnterOpensReader(t *testing.T) {
	tm := newThreadsModel(stubFetcher(map[string]json.RawMessage{
		"thread_list": mustJSON([]daemon.ThreadListItem{{ID: 500, Title: "t", Status: "done"}}),
		"read_thread": mustJSON([]daemon.Message{{ID: 500, FromProject: "co/parent", Content: "do it"}}),
	}))
	tm.refreshNow()
	tm2, _ := tm.update(tea.KeyMsg{Type: tea.KeyEnter})
	if !tm2.readerOpen {
		t.Fatal("enter must open reader")
	}
	if !strings.Contains(tm2.view(80, 24), "do it") {
		t.Fatal("reader must show messages")
	}
}

func TestThreadsNavAndEsc(t *testing.T) {
	tm := newThreadsModel(stubFetcher(map[string]json.RawMessage{
		"thread_list": mustJSON([]daemon.ThreadListItem{
			{ID: 1, Title: "a", Status: "idle"},
			{ID: 2, Title: "b", Status: "done"},
		}),
		"read_thread": mustJSON([]daemon.Message{{ID: 2, Content: "msg b"}}),
	}))
	tm.refreshNow()
	tm, _ = tm.update(tea.KeyMsg{Type: tea.KeyDown})
	if tm.sel != 1 {
		t.Fatal("down must move selection")
	}
	tm2, _ := tm.update(tea.KeyMsg{Type: tea.KeyEnter})
	if !tm2.readerOpen {
		t.Fatal("enter must open reader")
	}
	tm3, _ := tm2.update(tea.KeyMsg{Type: tea.KeyEsc})
	if tm3.readerOpen {
		t.Fatal("esc must close reader")
	}
}

func TestThreadsDeleteConfirm(t *testing.T) {
	var deletedID string
	tf := stubFetcher(map[string]json.RawMessage{
		"thread_list": mustJSON([]daemon.ThreadListItem{{ID: 500, Title: "t", Status: "idle"}}),
	})
	tf.threadDelete = func(params json.RawMessage) (json.RawMessage, error) {
		deletedID = string(params)
		return mustJSON(map[string]int{"deleted": 1}), nil
	}
	tm := newThreadsModel(tf)
	tm.refreshNow()

	tm, _ = tm.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	if !tm.confirm {
		t.Fatal("d must ask confirm")
	}
	tm, _ = tm.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	if tm.confirm || deletedID != "" {
		t.Fatal("n must cancel without deleting")
	}

	tm, _ = tm.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	tm, _ = tm.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	if !strings.Contains(deletedID, "500") {
		t.Fatalf("y must call thread_delete with thread_id 500, got %q", deletedID)
	}
	if tm.confirm {
		t.Fatal("confirm must clear after delete")
	}
}
