package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
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

type monitorRow struct {
	Name       string
	Project    string
	Status     string
	ThreadID   int64
	CreatedAt  string
	FinishedAt string
	TodosDone  int
	TodosTotal int
	Todos      []todoItem
	LastUpdate string
	Replies    []threadReply
}

type threadReply struct {
	From    string
	Status  string
	Content string
}

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
	var all bool
	c := &cobra.Command{
		Use:   "monitor",
		Short: "Live view of delegated tasks and their progress",
		RunE: func(cmd *cobra.Command, args []string) error {
			// No terminal (piped, redirected, CI) — bubbletea can't run, so
			// print one plain snapshot instead of failing.
			if !isTerminal(os.Stdout) {
				return printMonitorSnapshot(os.Stdout, all)
			}
			_, err := tea.NewProgram(newMonitorModel(all), tea.WithAltScreen()).Run()
			return err
		},
	}
	c.Flags().BoolVar(&all, "all", false, "include finished sessions (default: active only)")
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
	scope := "active"
	if m.all {
		scope = "all"
	}
	status := ""
	if m.loading {
		status = " · refreshing"
	}
	return styTitle.Render("hmf monitor") + " " +
		styDim.Render(fmt.Sprintf("· %d %s%s · %s", len(m.rows), scope, status, time.Now().Format("15:04:05"))) +
		"\n" + styDim.Render(help) + "\n\n"
}

func (m monitorModel) listView() string {
	help := keyHelp(
		"↑↓", "move", "enter", "open", "a", "all/active", "r", "refresh", "q", "quit",
	)
	var b strings.Builder
	b.WriteString(m.header(help))

	if m.err != nil {
		b.WriteString(styFailed.Render("daemon unreachable") + " — " + m.err.Error() + "\n")
		return b.String()
	}
	if len(m.rows) == 0 {
		b.WriteString(styDim.Render("nothing running") + "\n")
		return b.String()
	}

	projW := len("PROJECT")
	idW := len("ID")
	for _, r := range m.rows {
		projW = max(projW, lipgloss.Width(r.Project))
		idW = max(idW, len(fmt.Sprint(r.ThreadID)))
	}

	b.WriteString(styLabel.Render(fmt.Sprintf("  %-*s  %-7s  %-*s  %-9s  %s",
		idW, "ID", "STATUS", projW, "PROJECT", "ELAPSED", "TODOS")) + "\n")

	// One line per row: details belong in the detail view, not here.
	visible := max(1, m.h-7)
	start := clamp(m.cursor-visible/2, 0, max(0, len(m.rows)-visible))
	end := min(len(m.rows), start+visible)

	for i := start; i < end; i++ {
		r := m.rows[i]
		todos := styDim.Render(" – ")
		if r.TodosTotal > 0 {
			todos = fmt.Sprintf("%d/%d", r.TodosDone, r.TodosTotal)
		}
		line := fmt.Sprintf("%-*d  %s  %-*s  %-9s  %s",
			idW, r.ThreadID, statusLabel(r.Status), projW, r.Project,
			elapsed(r.CreatedAt, r.FinishedAt, r.Status == "active"), todos)

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
	head := styTitle.Render(r.Project) + " " +
		styDim.Render("· "+r.Status+" · "+elapsed(r.CreatedAt, r.FinishedAt, r.Status == "active")+" · thread "+fmt.Sprint(r.ThreadID)) +
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

	b.WriteString(styLabel.Render("session ") + r.Name + "\n\n")

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

	if len(r.Replies) == 0 {
		b.WriteString(styDim.Render("no replies yet") + "\n")
		return b.String()
	}
	b.WriteString(styLabel.Render("thread") + "\n")
	width := max(40, m.w-4)
	for _, rep := range r.Replies {
		who := rep.From
		if who == "" {
			who = "task"
		}
		tag := ""
		if rep.Status == "done" {
			tag = " " + styWorking.Render("[done]")
		}
		b.WriteString("\n  " + styKey.Render(who) + tag + "\n")
		for _, line := range wrapText(rep.Content, width-4) {
			b.WriteString("    " + line + "\n")
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

	// One row per task, not per spawn. A retried or resumed task has several
	// sessions on the same thread; session_list is newest-first, so the first
	// one we see is the current state.
	seen := map[int64]bool{}
	var rows []monitorRow
	for _, s := range sessions {
		if !all && s.Status != "active" {
			continue
		}
		if s.ParentID != 0 {
			if seen[s.ParentID] {
				continue
			}
			seen[s.ParentID] = true
		}
		if len(rows) >= monitorMaxRows {
			break
		}
		row := monitorRow{
			Name: s.Name, Project: s.Project, Status: s.Status,
			ThreadID: s.ParentID, CreatedAt: s.CreatedAt, FinishedAt: s.FinishedAt,
		}
		if s.ParentID != 0 {
			row.Todos = fetchTodos(s.ParentID)
			for _, t := range row.Todos {
				row.TodosTotal++
				if t.State == "done" {
					row.TodosDone++
				}
			}
			row.Replies = fetchReplies(s.ParentID)
			for i := len(row.Replies) - 1; i >= 0; i-- {
				row.LastUpdate = firstLine(row.Replies[i].Content)
				break
			}
		}
		rows = append(rows, row)
	}
	// Newest first — the thing you just dispatched should be at the top.
	sort.Slice(rows, func(i, j int) bool { return rows[i].CreatedAt > rows[j].CreatedAt })
	return rows, nil
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

// fetchReplies returns the thread's replies, oldest first. The root is the
// dispatcher's own task text and is skipped — it says nothing about progress.
func fetchReplies(threadID int64) []threadReply {
	result, err := protocol.Call("read_thread", map[string]any{"message_id": threadID})
	if err != nil {
		return nil
	}
	var msgs []struct {
		ThreadID    int64  `json:"thread_id"`
		FromProject string `json:"from_project"`
		Status      string `json:"status"`
		Content     string `json:"content"`
	}
	if json.Unmarshal(result, &msgs) != nil {
		return nil
	}
	var out []threadReply
	for _, msg := range msgs {
		if msg.ThreadID == 0 {
			continue // thread root
		}
		out = append(out, threadReply{From: msg.FromProject, Status: msg.Status, Content: msg.Content})
	}
	return out
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
	scope := "active"
	if all {
		scope = "all"
	}
	fmt.Fprintf(out, "hmf monitor · %d %s · %s\n\n", len(rows), scope, time.Now().Format("15:04:05"))
	if len(rows) == 0 {
		fmt.Fprintln(out, "nothing running")
		return nil
	}
	projW, idW := len("PROJECT"), len("ID")
	for _, r := range rows {
		projW = max(projW, len(r.Project))
		idW = max(idW, len(fmt.Sprint(r.ThreadID)))
	}
	fmt.Fprintf(out, "%-*s  %-7s  %-*s  %-9s  %s\n",
		idW, "ID", "STATUS", projW, "PROJECT", "ELAPSED", "TODOS")
	for _, r := range rows {
		todos := "-"
		if r.TodosTotal > 0 {
			todos = fmt.Sprintf("%d/%d", r.TodosDone, r.TodosTotal)
		}
		status := map[string]string{"active": "working", "exited": "done", "failed": "failed"}[r.Status]
		if status == "" {
			status = r.Status
		}
		fmt.Fprintf(out, "%-*d  %-7s  %-*s  %-9s  %s\n",
			idW, r.ThreadID, status, projW, r.Project, elapsed(r.CreatedAt, r.FinishedAt, r.Status == "active"), todos)
	}
	return nil
}
