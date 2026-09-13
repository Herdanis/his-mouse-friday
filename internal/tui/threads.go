package tui

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/herdanis/his-mouse-friday/internal/daemon"
	"github.com/herdanis/his-mouse-friday/internal/protocol"
)

// ============================================
// Threads panel
// ============================================

// threadsMsg carries a refreshed thread_list payload to the panel.
type threadsMsg struct {
	rows []daemon.ThreadListItem
}

type threadsModel struct {
	list func() (json.RawMessage, error)
	read func(json.RawMessage) (json.RawMessage, error)
	del  func(json.RawMessage) (json.RawMessage, error)

	rows    []daemon.ThreadListItem
	sel     int
	confirm bool
	// confirmReader remembers the confirm was armed while reading, so a
	// confirmed delete also drops back to the list.
	confirmReader bool

	readerOpen bool
	msgs       []daemon.Message
	rerr       string
	vp         viewport.Model
}

func newThreadsModel(f fetchers) threadsModel {
	return threadsModel{list: f.threadList, read: f.readThread, del: f.threadDelete}
}

// refreshNow does a synchronous thread_list fetch — tests and post-delete reload.
func (t *threadsModel) refreshNow() {
	raw, err := t.list()
	if err != nil {
		t.rerr = err.Error()
		return
	}
	var rows []daemon.ThreadListItem
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.rerr = err.Error()
		return
	}
	t.rows = rows
	t.sel = min(t.sel, max(0, len(rows)-1))
	t.rerr = ""
}

func (t threadsModel) refresh() tea.Cmd {
	return func() tea.Msg {
		raw, err := t.list()
		if err != nil {
			return fetchMsg{err: err}
		}
		var rows []daemon.ThreadListItem
		if err := json.Unmarshal(raw, &rows); err != nil {
			return fetchMsg{err: err}
		}
		return threadsMsg{rows: rows}
	}
}

func paramFetcher(method string) func(json.RawMessage) (json.RawMessage, error) {
	return func(params json.RawMessage) (json.RawMessage, error) {
		return protocol.Call(method, params)
	}
}

func (t threadsModel) update(msg tea.Msg) (threadsModel, tea.Cmd) {
	switch msg := msg.(type) {
	case threadsMsg:
		t.rows = msg.rows
		t.sel = min(t.sel, max(0, len(t.rows)-1))
		t.rerr = ""
		return t, nil
	case tea.KeyMsg:
		return t.keyUpdate(msg)
	}
	return t, nil
}

func (t threadsModel) capturing() bool { return t.confirm }

func (t threadsModel) keyUpdate(msg tea.KeyMsg) (threadsModel, tea.Cmd) {
	// Confirm must precede the reader branch: a confirm armed from either
	// mode consumes the y/n.
	if t.confirm {
		t.confirm = false
		wasReader := t.confirmReader
		t.confirmReader = false
		if msg.String() != "y" {
			return t, nil
		}
		if len(t.rows) == 0 {
			return t, nil
		}
		params, _ := json.Marshal(map[string]int64{"thread_id": t.rows[t.sel].ID})
		if _, err := t.del(params); err != nil {
			t.rerr = err.Error()
			return t, nil
		}
		if wasReader {
			t.readerOpen = false
		}
		t.refreshNow()
		return t, nil
	}
	if t.readerOpen {
		switch msg.String() {
		case "esc":
			t.readerOpen = false
			return t, nil
		case "d":
			t.confirm = true
			t.confirmReader = true
			return t, nil
		}
		var cmd tea.Cmd
		t.vp, cmd = t.vp.Update(msg)
		return t, cmd
	}
	switch msg.String() {
	case "up", "k":
		t.sel = max(0, t.sel-1)
	case "down", "j":
		t.sel = min(len(t.rows)-1, t.sel+1)
	case "enter":
		return t.openReader()
	case "d":
		if len(t.rows) > 0 {
			t.confirm = true
			t.confirmReader = false
		}
	}
	return t, nil
}

// openReader loads the thread synchronously — the RPC has a short timeout and
// there is nothing else to draw meanwhile.
func (t threadsModel) openReader() (threadsModel, tea.Cmd) {
	if len(t.rows) == 0 {
		return t, nil
	}
	params, _ := json.Marshal(map[string]int64{"message_id": t.rows[t.sel].ID})
	raw, err := t.read(params)
	if err != nil {
		t.rerr = err.Error()
		return t, nil
	}
	if err := json.Unmarshal(raw, &t.msgs); err != nil {
		t.rerr = err.Error()
		return t, nil
	}
	t.readerOpen = true
	t.vp = viewport.New(80, 20)
	t.vp.GotoTop()
	return t, nil
}

func (t threadsModel) view(w, h int) string {
	if t.readerOpen {
		return t.readerView(w, h)
	}
	if t.rerr != "" {
		return "\n  " + styFailed.Render(t.rerr)
	}
	if len(t.rows) == 0 {
		return "\n  " + styDim.Render("no threads")
	}
	var b strings.Builder
	for i, r := range t.rows {
		marker := "  "
		if i == t.sel {
			marker = "> "
		}
		b.WriteString(marker + rowLine(r) + "\n")
	}
	if t.confirm && len(t.rows) > 0 {
		b.WriteString(" " + styFailed.Render(fmt.Sprintf("delete thread #%d? y/n", t.rows[t.sel].ID)))
	}
	return b.String()
}

func (t threadsModel) readerView(w, h int) string {
	vp := t.vp
	vp.Width, vp.Height = w, max(1, h-1)
	lines := make([]string, 0, len(t.msgs))
	for _, m := range t.msgs {
		lines = append(lines, fmt.Sprintf("%s %s → %s: %s",
			m.TS.Format("15:04"), m.FromProject, m.ToProject, firstLine(m.Content, 200)))
	}
	vp.SetContent(strings.Join(lines, "\n"))
	out := vp.View() + "\n" + styDim.Render("esc back · j/k scroll · d delete")
	if t.confirm && len(t.rows) > 0 {
		out += "  " + styFailed.Render(fmt.Sprintf("delete thread #%d? y/n", t.rows[t.sel].ID))
	}
	return out
}

func statusStyle(s string) lipgloss.Style {
	switch s {
	case "working":
		return styWorking
	case "blocked":
		return styAttn
	case "done":
		return styDone
	default:
		return styDim
	}
}

func rowLine(r daemon.ThreadListItem) string {
	to := r.To
	if to == "" {
		to = "-"
	}
	return fmt.Sprintf("%s #%d %s %s → %s %d %s",
		statusStyle(r.Status).Render("●"), r.ID, r.Title, r.From, to, r.MsgCount, ageOf(r.LastTS))
}

func ageOf(ts string) string {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return "?"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func firstLine(s string, maxRunes int) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	r := []rune(s)
	if len(r) > maxRunes {
		r = append(r[:maxRunes:maxRunes], '…')
	}
	return string(r)
}
