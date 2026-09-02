package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/herdanis/his-mouse-friday/internal/protocol"
	"github.com/spf13/cobra"
)

// ============================================
// hmf monitor — live view of delegated work
// ============================================

// Children run as separate processes and can't report into the session that
// dispatched them, so this is the window into them: every task with its work
// items, and a scrollable detail view for one task's full history.

const (
	monitorEvery   = 2 * time.Second
	monitorMaxRows = 40 // detail costs a socket round-trip per thread
)

var (
	styTitle   = lipgloss.NewStyle().Bold(true)
	styDim     = lipgloss.NewStyle().Faint(true)
	styWorking = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	styFailed  = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	stySel     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))
	styKey     = lipgloss.NewStyle().Foreground(lipgloss.Color("4"))
	styLabel   = lipgloss.NewStyle().Faint(true)
)

type todoItem struct {
	Content string
	State   string
}

// monitorRow is one parent task: the thread you dispatched, plus every
// session spawned under it. A task can involve several projects (a backend
// agent delegating to a frontend one) and several attempts (a retry or
// resume), and all of that belongs to the same parent.
type monitorRow struct {
	ThreadID   int64
	Task       string // the dispatched instruction (thread root)
	From       string // who dispatched it — the parent
	Status     string // aggregate across attempts
	CreatedAt  string // first attempt started
	FinishedAt string // last attempt ended; empty while any is running
	Attempts   []attempt
	TodosDone  int
	TodosTotal int
	Todos      []todoItem
	LastUpdate string
	Events     []convEvent
}

// attempt is one spawned session working on a parent task.
type attempt struct {
	Name       string
	Project    string
	Status     string
	CreatedAt  string
	FinishedAt string
}

// Projects lists the distinct projects that worked on this task, in the order
// they first appear.
func (r monitorRow) Projects() []string {
	seen := map[string]bool{}
	var out []string
	for _, a := range r.Attempts {
		if !seen[a.Project] {
			seen[a.Project] = true
			out = append(out, a.Project)
		}
	}
	return out
}

// FromLabel names the parent that dispatched this conversation.
func (r monitorRow) FromLabel() string { return shortIdentity(r.From) }

// shortIdentity renders who someone is in one column's width:
// "workspace/project" → project, "dir:name" → name/, unknown → you.
func shortIdentity(id string) string {
	switch {
	case id == "":
		return "you"
	case strings.HasPrefix(id, "dir:"):
		return strings.TrimPrefix(id, "dir:") + "/" // trailing / marks a plain directory
	default:
		if i := strings.IndexByte(id, '/'); i >= 0 {
			return id[i+1:] // drop the workspace prefix; it is noise here
		}
		return id
	}
}

func (r monitorRow) ProjectLabel() string {
	p := r.Projects()
	switch len(p) {
	case 0:
		return "-"
	case 1:
		return p[0]
	default:
		return fmt.Sprintf("%s +%d", p[0], len(p)-1)
	}
}

// convEvent is one message in a parent↔child conversation.
type convEvent struct {
	TS       string
	From     string // empty = a human working from an unregistered dir
	To       string
	Status   string
	Content  string
	Dispatch bool
}

// Dispatch is set when the parent sent this message (asking for work) rather
// than a worker replying. It cannot be inferred from `to` alone: replies also
// carry a `to`, since hmf auto-fills it back to the originator.

func (e convEvent) Who() string { return shortIdentity(e.From) }

// Current is the work item the child is on — the first one not done.
func (r monitorRow) Current() string {
	for _, t := range r.Todos {
		if t.State != "done" {
			return t.Content
		}
	}
	return ""
}

func monitorCmd() *cobra.Command {
	var activeOnly bool
	c := &cobra.Command{
		Use:   "monitor",
		Short: "Live view of delegated tasks and their progress",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Shows everything by default: filtering to active-only greets you
			// with "nothing running" most of the time, since tasks finish fast.
			// Running work sorts to the top instead, and `a` filters.
			all := !activeOnly
			// No terminal (piped, redirected, CI) — bubbletea can't run, so
			// print one plain snapshot instead of failing.
			if !isTerminal(os.Stdout) {
				return printMonitorSnapshot(os.Stdout, all)
			}
			_, err := tea.NewProgram(newMonitorModel(all), tea.WithAltScreen()).Run()
			return err
		},
	}
	c.Flags().BoolVar(&activeOnly, "active", false, "show only tasks still running")
	return c
}

// ============================================
// Model
// ============================================

type rowsMsg struct {
	rows []monitorRow
	err  error
}
type tickMsg time.Time

type monitorModel struct {
	rows     []monitorRow
	cursor   int
	offset   int // first visible row, for scrolling
	detail   int // index being viewed; -1 = list
	all      bool
	err      error
	w, h     int
	vp       viewport.Model
	vpReady  bool
	loading  bool
	lastLoad time.Time
}

func newMonitorModel(all bool) monitorModel {
	return monitorModel{all: all, detail: -1, loading: true}
}

func (m monitorModel) Init() tea.Cmd {
	return tea.Batch(fetchRows(m.all), tickCmd())
}

func fetchRows(all bool) tea.Cmd {
	return func() tea.Msg {
		rows, err := collectMonitorRows(all)
		return rowsMsg{rows: rows, err: err}
	}
}

func tickCmd() tea.Cmd {
	return tea.Tick(monitorEvery, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m monitorModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		vh := max(3, m.h-6)
		if !m.vpReady {
			m.vp = viewport.New(msg.Width, vh)
			m.vpReady = true
		} else {
			m.vp.Width, m.vp.Height = msg.Width, vh
		}
		if m.detail >= 0 {
			m.vp.SetContent(m.detailBody())
		}

	case rowsMsg:
		m.loading = false
		m.lastLoad = time.Now()
		m.rows, m.err = msg.rows, msg.err
		if m.cursor >= len(m.rows) {
			m.cursor = max(0, len(m.rows)-1)
		}
		if m.detail >= len(m.rows) {
			m.detail = -1
		}
		if m.detail >= 0 && m.vpReady {
			// Keep the scroll position while content refreshes underneath.
			at := m.vp.YOffset
			m.vp.SetContent(m.detailBody())
			m.vp.YOffset = min(at, max(0, m.vp.TotalLineCount()-m.vp.Height))
		}

	case tickMsg:
		var cmds []tea.Cmd
		cmds = append(cmds, tickCmd())
		if !m.loading {
			m.loading = true
			cmds = append(cmds, fetchRows(m.all))
		}
		return m, tea.Batch(cmds...)

	case tea.KeyMsg:
		return m.handleKey(msg)
	}

	if m.detail >= 0 && m.vpReady {
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m monitorModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit

	case "esc", "h", "left":
		if m.detail >= 0 {
			m.detail = -1
		}
		return m, nil

	case "enter", "l", "right":
		if m.detail < 0 && len(m.rows) > 0 {
			m.detail = m.cursor
			if m.vpReady {
				m.vp.SetContent(m.detailBody())
				m.vp.GotoTop()
			}
		}
		return m, nil

	case "a":
		// Toggle scope without leaving the view.
		m.all = !m.all
		m.detail, m.cursor, m.offset = -1, 0, 0
		m.loading = true
		return m, fetchRows(m.all)

	case "r":
		if !m.loading {
			m.loading = true
			return m, fetchRows(m.all)
		}
		return m, nil
	}

	if m.detail >= 0 {
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		return m, cmd
	}

	switch msg.String() {
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		if m.cursor < len(m.rows)-1 {
			m.cursor++
		}
	case "g", "home":
		m.cursor = 0
	case "G", "end":
		m.cursor = max(0, len(m.rows)-1)
	}
	return m, nil
}

// ============================================
// View
// ============================================

func (m monitorModel) View() string {
	if m.detail >= 0 && m.detail < len(m.rows) {
		return m.detailView()
	}
	return m.listView()
}

func (m monitorModel) header(help string) string {
	scope := "tasks"
	if !m.all {
		scope = "running"
	}
	working := 0
	for _, r := range m.rows {
		if r.Status == "active" {
			working++
		}
	}
	summary := fmt.Sprintf("· %d %s", len(m.rows), scope)
	if m.all && working > 0 {
		summary += fmt.Sprintf(" · %d working", working)
	}
	if m.loading {
		summary += " · refreshing"
	}
	return styTitle.Render("hmf monitor") + " " +
		styDim.Render(summary+" · "+time.Now().Format("15:04:05")) +
		"\n" + styDim.Render(help) + "\n\n"
}

func (m monitorModel) listView() string {
	help := keyHelp(
		"↑↓", "move", "enter", "open", "a", "filter", "r", "refresh", "q", "quit",
	)
	var b strings.Builder
	b.WriteString(m.header(help))

	if m.err != nil {
		b.WriteString(styFailed.Render("daemon unreachable") + " — " + m.err.Error() + "\n")
		return b.String()
	}
	if len(m.rows) == 0 {
		b.WriteString(styDim.Render("no tasks yet — dispatch one with post_message") + "\n")
		return b.String()
	}

	idW, fromW := len("ID"), len("FROM")
	for _, r := range m.rows {
		idW = max(idW, len(fmt.Sprint(r.ThreadID)))
		fromW = max(fromW, lipgloss.Width(r.FromLabel()))
	}
	// Whatever is left goes to the task summary — the one field that actually
	// tells rows apart. Who did the work can be several projects, so that
	// lives in the detail view rather than being flattened into a column.
	taskW := max(20, m.w-(idW+fromW+7+9+6+12))

	b.WriteString(styLabel.Render(fmt.Sprintf("  %-*s  %-7s  %-*s  %-9s  %-5s  %s",
		idW, "ID", "STATUS", fromW, "FROM", "ELAPSED", "TODOS", "TASK")) + "\n")

	// One line per row: details belong in the detail view, not here.
	visible := max(1, m.h-7)
	start := clamp(m.cursor-visible/2, 0, max(0, len(m.rows)-visible))
	end := min(len(m.rows), start+visible)

	for i := start; i < end; i++ {
		r := m.rows[i]
		todos := styDim.Render(padTo("–", 5))
		if r.TodosTotal > 0 {
			todos = padTo(fmt.Sprintf("%d/%d", r.TodosDone, r.TodosTotal), 5)
		}
		line := fmt.Sprintf("%-*d  %s  %-*s  %-9s  %s  %s",
			idW, r.ThreadID, statusLabel(r.Status), fromW, r.FromLabel(),
			elapsed(r.CreatedAt, r.FinishedAt, r.Status == "active"),
			todos, truncate(firstLine(r.Task), taskW))

		if i == m.cursor {
			b.WriteString(stySel.Render("▸ ") + lipgloss.NewStyle().Bold(true).Render(line) + "\n")
		} else {
			b.WriteString("  " + line + "\n")
		}
	}

	if len(m.rows) > visible {
		b.WriteString(styDim.Render(fmt.Sprintf("\n  showing %d–%d of %d", start+1, end, len(m.rows))))
	}
	return b.String()
}

func (m monitorModel) detailView() string {
	r := m.rows[m.detail]
	help := keyHelp("↑↓", "scroll", "esc", "back", "r", "refresh", "q", "quit")
	head := styTitle.Render(fmt.Sprintf("thread %d", r.ThreadID)) + " " +
		styDim.Render("· "+r.Status+" · "+elapsed(r.CreatedAt, r.FinishedAt, r.Status == "active")) +
		"\n" + styDim.Render(help) + "\n\n"
	if !m.vpReady {
		return head + m.detailBody()
	}
	return head + m.vp.View()
}

// detailBody is everything known about one task — its work items and the full
// back-and-forth on its thread.
func (m monitorModel) detailBody() string {
	if m.detail < 0 || m.detail >= len(m.rows) {
		return ""
	}
	r := m.rows[m.detail]
	var b strings.Builder
	width := max(40, m.w-4)

	// Who dispatched this — the parent. Empty means a human from an
	// unregistered directory rather than another project's agent.
	from := r.From
	switch {
	case from == "":
		from = "you (unregistered dir)"
	case strings.HasPrefix(from, "dir:"):
		from = strings.TrimPrefix(from, "dir:") + "/ " + styDim.Render("(unregistered dir)")
	}
	// Name every worker here rather than a "+N" summary — the detail view is
	// exactly where the full list belongs.
	workers := strings.Join(r.Projects(), ", ")
	if workers == "" {
		workers = "-"
	}
	b.WriteString(styLabel.Render("from ") + from + styDim.Render("  →  ") + workers + "\n\n")

	if r.Task != "" {
		b.WriteString(styLabel.Render("task") + "\n")
		for _, line := range wrapText(r.Task, width-4) {
			b.WriteString("  " + styDim.Render(line) + "\n")
		}
		b.WriteString("\n")
	}

	// Attempts: a task can be retried, resumed, or handed between projects —
	// the list view collapses that to one line, so show it in full here.
	if len(r.Attempts) > 1 || (len(r.Attempts) == 1 && len(r.Projects()) > 1) {
		b.WriteString(styLabel.Render(fmt.Sprintf("attempts · %d", len(r.Attempts))) + "\n")
		for i, a := range r.Attempts {
			b.WriteString(fmt.Sprintf("  %d. %s  %s  %s\n",
				i+1, statusLabel(a.Status), a.Project,
				styDim.Render(elapsed(a.CreatedAt, a.FinishedAt, a.Status == "active"))))
		}
		b.WriteString("\n")
	} else if len(r.Attempts) == 1 {
		b.WriteString(styLabel.Render("session ") + r.Attempts[0].Name + "\n\n")
	}

	if r.TodosTotal == 0 {
		b.WriteString(styDim.Render("no work items posted yet") + "\n\n")
	} else {
		b.WriteString(styLabel.Render(fmt.Sprintf("work items · %d/%d done", r.TodosDone, r.TodosTotal)) + "\n")
		for _, t := range r.Todos {
			mark := styDim.Render("·")
			text := styDim.Render(t.Content)
			if t.State == "done" {
				mark, text = styWorking.Render("✓"), styDim.Render(t.Content)
			} else if r.Status == "active" {
				mark, text = styKey.Render("▸"), t.Content
			}
			b.WriteString("  " + mark + " " + text + "\n")
		}
		b.WriteString("\n")
	}

	if len(r.Events) == 0 {
		b.WriteString(styDim.Render("no messages yet") + "\n")
		return b.String()
	}

	// The exchange itself, oldest first: who asked whom, and what came back.
	// Dispatches are indented left, replies right, so the direction reads at
	// a glance without hunting through the text.
	b.WriteString(styLabel.Render("conversation") + "\n")
	for _, e := range r.Events {
		when := ""
		if t, ok := parseDaemonTime(e.TS); ok {
			when = styDim.Render(t.Local().Format("15:04:05") + " ")
		}
		var head, indent string
		if e.Dispatch {
			head = when + styKey.Render(e.Who()) + styDim.Render(" → ") + e.To
			indent = "  "
		} else {
			tag := ""
			switch e.Status {
			case "done":
				tag = " " + styWorking.Render("[done]")
			case "ack":
				// Harness-generated, not the agent talking — label it so a
				// pickup notice is never mistaken for the child's own reply.
				tag = " " + styDim.Render("[hmf]")
			}
			head = when + styWorking.Render(e.Who()) + styDim.Render(" ↩") + tag
			indent = "      "
		}
		b.WriteString("\n" + indent + head + "\n")
		for _, line := range wrapText(e.Content, max(20, width-len(indent)-2)) {
			b.WriteString(indent + "  " + line + "\n")
		}
	}
	return b.String()
}

func keyHelp(pairs ...string) string {
	var parts []string
	for i := 0; i+1 < len(pairs); i += 2 {
		parts = append(parts, pairs[i]+" "+pairs[i+1])
	}
	return strings.Join(parts, " · ")
}

func statusLabel(s string) string {
	switch s {
	case "active":
		return styWorking.Render(fmt.Sprintf("%-7s", "working"))
	case "exited":
		return styDim.Render(fmt.Sprintf("%-7s", "done"))
	case "failed":
		return styFailed.Render(fmt.Sprintf("%-7s", "failed"))
	}
	return fmt.Sprintf("%-7s", s)
}

// ============================================
// Data
// ============================================

// collectMonitorRows joins the session list with each thread's todos and replies.
func collectMonitorRows(all bool) ([]monitorRow, error) {
	result, err := protocol.Call("session_list", struct{}{})
	if err != nil {
		return nil, err
	}
	var sessions []struct {
		Name       string `json:"name"`
		Project    string `json:"project"`
		Status     string `json:"status"`
		ParentID   int64  `json:"parent_id"`
		CreatedAt  string `json:"created_at"`
		FinishedAt string `json:"finished_at"`
	}
	if err := json.Unmarshal(result, &sessions); err != nil {
		return nil, fmt.Errorf("parse sessions: %w", err)
	}

	// Group by parent task. One thread can carry several attempts and several
	// projects (one agent delegating to another); they are all the same task,
	// so they belong on one row rather than as unrelated siblings.
	order := []int64{}
	byThread := map[int64]*monitorRow{}
	for _, s := range sessions {
		if s.ParentID == 0 {
			continue // no thread to group under
		}
		row := byThread[s.ParentID]
		if row == nil {
			if len(order) >= monitorMaxRows {
				continue
			}
			row = &monitorRow{ThreadID: s.ParentID}
			byThread[s.ParentID] = row
			order = append(order, s.ParentID)
		}
		row.Attempts = append(row.Attempts, attempt{
			Name: s.Name, Project: s.Project, Status: s.Status,
			CreatedAt: s.CreatedAt, FinishedAt: s.FinishedAt,
		})
	}

	var rows []monitorRow
	for _, id := range order {
		row := byThread[id]
		// session_list is newest-first; attempts read better oldest-first.
		slices.Reverse(row.Attempts)
		summariseAttempts(row)

		row.Todos = fetchTodos(id)
		for _, t := range row.Todos {
			row.TodosTotal++
			if t.State == "done" {
				row.TodosDone++
			}
		}
		row.Task, row.From, row.Events = fetchConversation(id)
		for i := len(row.Events) - 1; i >= 0; i-- {
			if !row.Events[i].Dispatch {
				row.LastUpdate = firstLine(row.Events[i].Content)
				break
			}
		}
		if !all && row.Status != "active" {
			continue
		}
		rows = append(rows, *row)
	}
	// Running work first, then newest. With everything on screen, the tasks
	// still in flight are what you opened this for — they must never be
	// buried under finished history.
	sort.SliceStable(rows, func(i, j int) bool {
		ai, aj := rows[i].Status == "active", rows[j].Status == "active"
		if ai != aj {
			return ai
		}
		return rows[i].CreatedAt > rows[j].CreatedAt
	})
	return rows, nil
}

// summariseAttempts rolls a task's attempts into one status and span: it is
// running if any attempt is, and its duration covers the first start to the
// last finish.
func summariseAttempts(row *monitorRow) {
	if len(row.Attempts) == 0 {
		return
	}
	row.CreatedAt = row.Attempts[0].CreatedAt
	last := row.Attempts[len(row.Attempts)-1]
	row.Status, row.FinishedAt = last.Status, last.FinishedAt
	for _, a := range row.Attempts {
		if a.Status == "active" {
			row.Status, row.FinishedAt = "active", "" // still running
			break
		}
	}
}

func fetchTodos(threadID int64) []todoItem {
	r, err := protocol.Call("todo_list", map[string]any{"thread_id": threadID})
	if err != nil {
		return nil
	}
	var td []struct {
		Content string `json:"content"`
		State   string `json:"state"`
	}
	if json.Unmarshal(r, &td) != nil {
		return nil
	}
	out := make([]todoItem, 0, len(td))
	for _, t := range td {
		out = append(out, todoItem{Content: t.Content, State: t.State})
	}
	return out
}

// fetchConversation returns the whole parent↔child exchange on a thread,
// oldest first: the dispatches the parent sent and the replies children made.
func fetchConversation(threadID int64) (task, from string, events []convEvent) {
	result, err := protocol.Call("read_thread", map[string]any{"message_id": threadID})
	if err != nil {
		return "", "", nil
	}
	var msgs []struct {
		ThreadID    int64  `json:"thread_id"`
		FromProject string `json:"from_project"`
		ToProject   string `json:"to_project"`
		Status      string `json:"status"`
		Content     string `json:"content"`
		TS          string `json:"ts"`
	}
	if json.Unmarshal(result, &msgs) != nil {
		return "", "", nil
	}
	if len(msgs) > 0 {
		task, from = msgs[0].Content, msgs[0].FromProject // the opening request
	}
	for _, m := range msgs {
		events = append(events, convEvent{
			TS: m.TS, From: m.FromProject, To: m.ToProject,
			Status: m.Status, Content: m.Content,
			// The parent is whoever opened the thread; anyone else is a worker
			// reporting back. Follow-ups from the parent stay dispatches.
			Dispatch: m.FromProject == from,
		})
	}
	return task, from, events
}

// ============================================
// Formatting helpers
// ============================================

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// elapsed reports how long a task ran: a live one counts up from its start, a
// finished one freezes at its real duration. Without the freeze the number
// keeps climbing long after the work is over, which reads as "still running".
//
// running is false for a task that has ended. If such a task has no recorded
// finish time (it ended before that was tracked and had no done reply to
// backfill from) its duration is unknown — say so rather than showing a
// number that keeps growing.
func elapsed(created, finished string, running bool) string {
	start, ok := parseDaemonTime(created)
	if !ok {
		return "-"
	}
	if t, ok := parseDaemonTime(finished); ok {
		return humanDuration(t.Sub(start))
	}
	if !running {
		return "?"
	}
	return humanDuration(time.Since(start))
}

// parseDaemonTime accepts the formats the daemon writes timestamps in.
func parseDaemonTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{
		"2006-01-02 15:04:05.999999999 -0700 MST",
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05",
		time.RFC3339Nano,
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func humanDuration(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d < 0:
		return "0s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// padTo pads to a visible width. Style the result, never the input: escape
// codes count toward fmt's width and would skew every column after it.
func padTo(s string, w int) string {
	if n := len([]rune(s)); n < w {
		return s + strings.Repeat(" ", w-n)
	}
	return s
}

func truncate(s string, max int) string {
	if r := []rune(strings.TrimSpace(s)); len(r) > max && max > 1 {
		return string(r[:max-1]) + "…"
	}
	return s
}

// wrapText breaks a long line on word boundaries so a chatty reply stays
// readable instead of running off the terminal.
func wrapText(s string, width int) []string {
	var out []string
	for _, para := range strings.Split(s, "\n") {
		line := ""
		for _, w := range strings.Fields(para) {
			switch {
			case line == "":
				line = w
			case len([]rune(line))+1+len([]rune(w)) <= width:
				line += " " + w
			default:
				out = append(out, line)
				line = w
			}
		}
		out = append(out, line)
	}
	return out
}

func clamp(v, lo, hi int) int { return max(lo, min(v, hi)) }

// isTerminal reports whether f is a character device, i.e. a real terminal
// rather than a pipe or file.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// printMonitorSnapshot writes one plain, uncoloured frame. Keeps
// `hmf monitor | tee` and scripted use working without a terminal.
func printMonitorSnapshot(out io.Writer, all bool) error {
	rows, err := collectMonitorRows(all)
	if err != nil {
		return err
	}
	scope := "tasks"
	if !all {
		scope = "running"
	}
	fmt.Fprintf(out, "hmf monitor · %d %s · %s\n\n", len(rows), scope, time.Now().Format("15:04:05"))
	if len(rows) == 0 {
		fmt.Fprintln(out, "no tasks")
		return nil
	}
	idW, fromW := len("ID"), len("FROM")
	for _, r := range rows {
		idW = max(idW, len(fmt.Sprint(r.ThreadID)))
		fromW = max(fromW, len(r.FromLabel()))
	}
	fmt.Fprintf(out, "%-*s  %-7s  %-*s  %-9s  %-5s  %s\n",
		idW, "ID", "STATUS", fromW, "FROM", "ELAPSED", "TODOS", "TASK")
	for _, r := range rows {
		todos := "-"
		if r.TodosTotal > 0 {
			todos = fmt.Sprintf("%d/%d", r.TodosDone, r.TodosTotal)
		}
		status := map[string]string{"active": "working", "exited": "done", "failed": "failed"}[r.Status]
		if status == "" {
			status = r.Status
		}
		fmt.Fprintf(out, "%-*d  %-7s  %-*s  %-9s  %-5s  %s\n",
			idW, r.ThreadID, status, fromW, r.FromLabel(),
			elapsed(r.CreatedAt, r.FinishedAt, r.Status == "active"),
			todos, truncate(firstLine(r.Task), 60))
	}
	return nil
}
