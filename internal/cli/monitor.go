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

// GitHub Primer palette, adaptive so the view reads on light and dark
// terminals. Foreground only — the terminal keeps its own background.
var (
	cAccent  = lipgloss.AdaptiveColor{Light: "#0969da", Dark: "#58a6ff"}
	cSuccess = lipgloss.AdaptiveColor{Light: "#1a7f37", Dark: "#3fb950"}
	cDanger  = lipgloss.AdaptiveColor{Light: "#cf222e", Dark: "#f85149"}
	cAttn    = lipgloss.AdaptiveColor{Light: "#9a6700", Dark: "#d29922"}
	cDoneFg  = lipgloss.AdaptiveColor{Light: "#8250df", Dark: "#a371f7"}
	cMuted   = lipgloss.AdaptiveColor{Light: "#59636e", Dark: "#8b949e"}
	cBorder  = lipgloss.AdaptiveColor{Light: "#d1d9e0", Dark: "#30363d"}
	cText    = lipgloss.AdaptiveColor{Light: "#1f2328", Dark: "#e6edf3"}
)

var (
	styTitle   = lipgloss.NewStyle().Bold(true).Foreground(cText)
	styText    = lipgloss.NewStyle().Foreground(cText)
	styDim     = lipgloss.NewStyle().Foreground(cMuted)
	styWorking = lipgloss.NewStyle().Foreground(cSuccess)
	styFailed  = lipgloss.NewStyle().Foreground(cDanger)
	styDone    = lipgloss.NewStyle().Foreground(cDoneFg)
	styAttn    = lipgloss.NewStyle().Foreground(cAttn)
	stySel     = lipgloss.NewStyle().Bold(true).Foreground(cAccent)
	styKey     = lipgloss.NewStyle().Foreground(cAccent)
	styLabel   = lipgloss.NewStyle().Bold(true).Foreground(cMuted)
	styRule    = lipgloss.NewStyle().Foreground(cBorder)
)

type todoItem struct {
	ID      int64
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
	EngagedBy  string // who asked for this work
	PID        int
	SessionID  string // opencode session id — what `opencode -s` takes
	Dir        string // project path; opencode resumes per-directory
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

// FailedAttempts counts spawns that ended badly. A task stays "working" while
// any attempt runs, so a failed sibling is otherwise invisible from the list.
func (r monitorRow) FailedAttempts() int {
	n := 0
	for _, a := range r.Attempts {
		if a.Status == "failed" {
			n++
		}
	}
	return n
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

	// split is set when the terminal is wide enough for list and detail side
	// by side; focus picks which of the two panes the keys drive.
	split bool
	focus int

	// Work-item selection inside the detail view. pickTodo turns ↑↓ into an
	// item cursor instead of scrolling; confirmDel guards the delete.
	pickTodo   bool
	todoIdx    int
	confirmDel bool
	notice     string
}

func newMonitorModel(all bool) monitorModel {
	return monitorModel{all: all, detail: -1, loading: true}
}

func (m monitorModel) Init() tea.Cmd {
	return tea.Batch(fetchRows(m.all), tickCmd())
}

// agentRun is one child agent: every spawn that shares its opencode session,
// rolled into a single entry. Resuming a session reuses its id, so a task
// picked up three times produced three session rows carrying one conversation
// — listing them separately implies three children that you could open
// separately, and you cannot.
type agentRun struct {
	attempt
	Runs   int
	Failed int
	Total  time.Duration
	Known  bool // at least one run had a measurable duration
}

// groupRuns collapses attempts per agent session, preserving what collapsing
// would otherwise hide: how many runs there were and whether any failed.
func groupRuns(as []attempt) []agentRun {
	var out []agentRun
	idx := map[string]int{}
	for _, a := range as {
		// Sessions whose id was never captured cannot be proven to be the same
		// conversation, so they stay separate rather than being merged on a
		// guess. Name is the next-best identity; failing that, never merge.
		key := a.SessionID
		if key == "" {
			key = "name:" + a.Name
		}
		if key == "" || key == "name:" || key == "name:-" {
			key = fmt.Sprintf("uniq:%d", len(out))
		}
		i, seen := idx[key]
		if !seen {
			idx[key] = len(out)
			out = append(out, agentRun{attempt: a})
			i = len(out) - 1
		}
		g := &out[i]
		g.Runs++
		if a.Status == "failed" {
			g.Failed++
		}
		// Latest run wins for status/pid: it is the current state of the agent.
		if a.Status == "active" || g.Status != "active" {
			g.Status, g.PID = a.Status, a.PID
		}
		if start, ok := parseDaemonTime(a.CreatedAt); ok {
			if end, ok := parseDaemonTime(a.FinishedAt); ok {
				g.Total += end.Sub(start)
				g.Known = true
			} else if a.Status == "active" {
				g.Total += time.Since(start)
				g.Known = true
			}
		}
	}
	return out
}

func (g agentRun) Duration() string {
	if !g.Known {
		return "?"
	}
	return humanDuration(g.Total)
}

// firstResumable returns an attempt whose session can actually be reopened,
// for the hint line. Sessions whose id was never captured cannot.
func firstResumable(as []attempt) *attempt {
	for i := range as {
		if as[i].SessionID != "" && as[i].Dir != "" {
			return &as[i]
		}
	}
	return nil
}

// refreshDetail re-renders the detail pane after state that changes its
// content (item cursor, confirm prompt, notice).
func (m *monitorModel) refreshDetail() tea.Cmd {
	if m.detail >= 0 && m.vpReady {
		m.vp.SetContent(m.detailBody())
	}
	return nil
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
		m.split = m.w >= splitMinWidth
		vw, vh := m.vpSize()
		if !m.vpReady {
			m.vp = viewport.New(vw, vh)
			// vim keys scroll the detail too; the list already answers to them.
			m.vp.KeyMap.Down.SetKeys("down", "j")
			m.vp.KeyMap.Up.SetKeys("up", "k")
			m.vpReady = true
		} else {
			m.vp.Width, m.vp.Height = vw, vh
		}
		if m.split && m.detail < 0 && len(m.rows) > 0 {
			m.detail = m.cursor
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
		if m.split && m.detail < 0 && len(m.rows) > 0 {
			m.detail = m.cursor
			if m.vpReady {
				m.vp.SetContent(m.detailBody())
				m.vp.GotoTop()
			}
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

	case "tab":
		if m.split {
			m.focus = 1 - m.focus
		}
		return m, nil

	case "esc", "h", "left":
		switch {
		case m.confirmDel:
			m.confirmDel = false
		case m.pickTodo:
			m.pickTodo = false
		case m.split && m.focus == 1:
			m.focus = 0
		case !m.split && m.detail >= 0:
			m.detail = -1
		}
		m.notice = ""
		return m, m.refreshDetail()

	case "enter", "l", "right":
		if m.split {
			m.focus = 1
			return m, nil
		}
		if m.detail < 0 && len(m.rows) > 0 {
			m.detail = m.cursor
			if m.vpReady {
				m.vp.SetContent(m.detailBody())
				m.vp.GotoTop()
			}
		}
		return m, nil

	case "d":
		// Work-item delete lives here because this is where you notice a
		// stale one — an orphaned item that can never be completed.
		if m.detail >= 0 && !m.confirmDel {
			r := m.rows[m.detail]
			if len(r.Todos) == 0 {
				m.notice = "no work items to delete"
				return m, m.refreshDetail()
			}
			if !m.pickTodo {
				m.pickTodo, m.todoIdx, m.notice = true, 0, ""
			} else {
				m.confirmDel = true
			}
			return m, m.refreshDetail()
		}
		return m, nil

	case "y":
		if m.confirmDel && m.detail >= 0 {
			r := m.rows[m.detail]
			if m.todoIdx < len(r.Todos) {
				if err := deleteTodo(r.Todos[m.todoIdx].ID); err != nil {
					m.notice = "delete failed: " + err.Error()
				} else {
					m.notice = "deleted work item"
				}
			}
			m.confirmDel, m.pickTodo = false, false
			m.loading = true
			return m, fetchRows(m.all)
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

	// Item selection owns ↑↓ while it is on, whichever pane has focus.
	if m.detail >= 0 && m.pickTodo {
		n := len(m.rows[m.detail].Todos)
		switch msg.String() {
		case "up", "k":
			if m.todoIdx > 0 {
				m.todoIdx--
			}
		case "down", "j":
			if m.todoIdx < n-1 {
				m.todoIdx++
			}
		}
		return m, m.refreshDetail()
	}

	if (m.split && m.focus == 1) || (!m.split && m.detail >= 0) {
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
	m.syncDetail()
	return m, nil
}

// syncDetail keeps the split-pane detail pointed at the highlighted row.
func (m *monitorModel) syncDetail() {
	if !m.split || len(m.rows) == 0 || m.detail == m.cursor {
		return
	}
	m.detail = m.cursor
	m.pickTodo, m.confirmDel, m.notice = false, false, ""
	if m.vpReady {
		m.vp.SetContent(m.detailBody())
		m.vp.GotoTop()
	}
}

// ============================================
// View
// ============================================

// Wide terminals get the task list and the selected task's detail side by
// side, so a running child's latest reply is visible without leaving the
// list. Narrow ones fall back to list → detail as separate screens.
const splitMinWidth = 96

func (m monitorModel) paneWidths() (int, int) {
	lw := clamp(m.w*38/100, 34, 56)
	return lw, m.w - lw
}

func (m monitorModel) paneHeight() int { return max(6, m.h-2) }

// vpSize is the detail viewport: pane minus its border, padding and header.
func (m monitorModel) vpSize() (int, int) {
	if m.split {
		_, rw := m.paneWidths()
		return max(20, rw-4), max(3, m.paneHeight()-5)
	}
	return max(20, m.w-4), max(3, m.h-6)
}

func (m monitorModel) bodyWidth() int {
	w, _ := m.vpSize()
	return max(40, w)
}

func (m monitorModel) View() string {
	if m.err != nil {
		return m.topBar() + "\n\n  " + styFailed.Render("daemon unreachable") +
			"  " + styDim.Render(m.err.Error()) + "\n\n" + m.footer()
	}
	if !m.split {
		if m.detail >= 0 && m.detail < len(m.rows) {
			return m.narrowDetail()
		}
		return m.narrowList()
	}
	return m.splitView()
}

func (m monitorModel) splitView() string {
	lw, rw := m.paneWidths()
	ph := m.paneHeight()
	left := pane(lw, ph, m.listBody(lw-4, ph-2), m.focus == 0)
	right := pane(rw, ph, m.detailPane(), m.focus == 1)
	return m.topBar() + "\n" +
		lipgloss.JoinHorizontal(lipgloss.Top, left, right) + "\n" + m.footer()
}

func (m monitorModel) narrowList() string {
	return m.topBar() + "\n" + m.listBody(max(20, m.w-2), max(2, m.h-3)) + "\n" + m.footer()
}

func (m monitorModel) narrowDetail() string {
	head := m.detailHead(m.rows[m.detail], max(20, m.w-2))
	body := m.detailBody()
	if m.vpReady {
		body = m.vp.View()
	}
	return m.topBar() + "\n" + head + "\n" + body + "\n" + m.footer()
}

func pane(w, h int, body string, focused bool) string {
	border := cBorder
	if focused {
		border = cAccent
	}
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).BorderForeground(border).
		Width(max(4, w-2)).Height(max(2, h-2)).Padding(0, 1).
		Render(body)
}

// topBar is the one line that answers "is anything running right now".
func (m monitorModel) topBar() string {
	var working, failed int
	for _, r := range m.rows {
		switch r.Status {
		case "active":
			working++
		case "failed":
			failed++
		}
	}
	scope := "tasks"
	if !m.all {
		scope = "running"
	}
	segs := []string{}
	if working > 0 {
		segs = append(segs, styWorking.Render(fmt.Sprintf("● %d working", working)))
	}
	if failed > 0 {
		segs = append(segs, styFailed.Render(fmt.Sprintf("✗ %d failed", failed)))
	}
	if len(m.rows) == 1 {
		scope = strings.TrimSuffix(scope, "s")
	}
	segs = append(segs, styDim.Render(fmt.Sprintf("%d %s", len(m.rows), scope)))

	left := " " + styTitle.Render("hmf monitor") + "  " + strings.Join(segs, styDim.Render(" · "))
	clock := time.Now().Format("15:04:05")
	if m.loading {
		clock += " ⟳"
	}
	right := styDim.Render(clock) + " "
	gap := m.w - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		return left
	}
	return left + strings.Repeat(" ", gap) + right
}

func (m monitorModel) footer() string {
	scope := "active only"
	if !m.all {
		scope = "show all"
	}
	pairs := []string{"↑↓", "move"}
	switch {
	case m.split:
		pairs = append(pairs, "tab", "focus")
	case m.detail >= 0:
		pairs = []string{"↑↓", "scroll", "esc", "back"}
	default:
		pairs = append(pairs, "enter", "open")
	}
	pairs = append(pairs, "d", "del item", "a", scope, "r", "refresh", "q", "quit")
	return " " + keyHelp(pairs...)
}

func keyHelp(pairs ...string) string {
	var parts []string
	for i := 0; i+1 < len(pairs); i += 2 {
		parts = append(parts, styKey.Render(pairs[i])+" "+styDim.Render(pairs[i+1]))
	}
	return strings.Join(parts, styDim.Render(" · "))
}

// ============================================
// List
// ============================================

// listBody renders two lines per task — identity on top, progress and the
// instruction below — so the list stays scannable at any pane width.
func (m monitorModel) listBody(w, h int) string {
	if len(m.rows) == 0 {
		return styDim.Render("no tasks yet") + "\n\n" +
			styDim.Render("dispatch one with post_message")
	}
	visible := max(1, h/2)
	if len(m.rows) > visible {
		visible = max(1, (h-1)/2) // the "n of m" line costs a row
	}
	start := clamp(m.cursor-visible/2, 0, max(0, len(m.rows)-visible))
	end := min(len(m.rows), start+visible)

	var b strings.Builder
	for i := start; i < end; i++ {
		b.WriteString(m.listEntry(i, w) + "\n")
	}
	if len(m.rows) > visible {
		b.WriteString(styDim.Render(fmt.Sprintf(" %d–%d of %d", start+1, end, len(m.rows))))
	}
	return strings.TrimRight(b.String(), "\n")
}

func (m monitorModel) listEntry(i, w int) string {
	r := m.rows[i]
	sel := i == m.cursor

	mark, markSty := statusMark(r.Status)
	lead, name := "  ", styDim
	if sel {
		lead, name = stySel.Render("▎")+" ", styText.Bold(true)
	}

	el := r.WorkElapsed()
	inner := max(8, w-2) // minus the selection lead
	// The dispatcher, not the worker: only one name fits at this width, and
	// the parent is the stable identity — it is there before anything spawns
	// and does not change when the task fans out. The worker is named in the
	// detail header, which the narrow layout drops.
	who := r.FromLabel()
	head := fmt.Sprintf("#%d  %s", r.ThreadID, who)
	headW := max(4, inner-2-lipgloss.Width(el)-1)
	head = padTo(truncate(head, headW), headW)
	line1 := lead + markSty.Render(mark) + " " + name.Render(head) + " " + styDim.Render(el)

	counts := "—"
	if r.TodosTotal > 0 {
		counts = fmt.Sprintf("%d/%d", r.TodosDone, r.TodosTotal)
	}
	counts = padTo(counts, 5)
	// A task reads as running while one of its agents has already failed —
	// say so here rather than only inside the session tree.
	fail := ""
	if n := r.FailedAttempts(); n > 0 && r.Status != "failed" {
		fail = styFailed.Render(fmt.Sprintf("✗%d ", n))
	}
	task := truncate(firstLine(r.Task), max(4, inner-12-lipgloss.Width(fail)))
	line2 := lead + progressBar(r.TodosDone, r.TodosTotal, 5) + " " +
		styDim.Render(counts) + " " + fail + styDim.Render(task)

	return line1 + "\n" + line2
}

func progressBar(done, total, w int) string {
	if total <= 0 {
		return styRule.Render(strings.Repeat("·", w))
	}
	filled := clamp(done*w/total, 0, w)
	if done > 0 && filled == 0 {
		filled = 1 // any progress at all must be visible
	}
	return styWorking.Render(strings.Repeat("▰", filled)) +
		styRule.Render(strings.Repeat("▱", w-filled))
}

func statusMark(s string) (string, lipgloss.Style) {
	switch s {
	case "active":
		return "●", styWorking
	case "exited":
		return "✓", styDone
	case "failed":
		return "✗", styFailed
	}
	return "○", styDim
}

func statusWord(s string) string {
	switch s {
	case "active":
		return "working"
	case "exited":
		return "done"
	case "failed":
		return "failed"
	}
	return s
}

func statusLabel(s string) string {
	mark, sty := statusMark(s)
	return sty.Render(mark + " " + padTo(statusWord(s), 7))
}

// ============================================
// Session tree
// ============================================

// sessionTree lays each agent under whoever engaged it, so a hand-off
// (A engages B, B engages C) reads as a chain rather than a flat list of
// unrelated attempts.
//
// ponytail: two agents in the same project share a key, so the second nests
// under the first's parent. Per-session parent ids would fix it; the daemon
// records engaged_by by project, and one project working a thread twice in
// parallel has not happened yet.
func sessionTree(runs []agentRun, root string) []string {
	known := map[string]bool{root: true}
	for _, g := range runs {
		known[g.Project] = true
	}
	// An engager nobody recognises (or an agent naming itself) hangs off the
	// dispatcher — better a flat entry than a session dropped from the tree.
	kids := map[string][]int{}
	for i, g := range runs {
		p := shortIdentity(g.EngagedBy)
		if p == "" || p == g.Project || !known[p] {
			p = root
		}
		kids[p] = append(kids[p], i)
	}

	var out []string
	seen := make([]bool, len(runs))
	var walk func(key string, depth int)
	walk = func(key string, depth int) {
		list := kids[key]
		for n, i := range list {
			if seen[i] {
				continue
			}
			seen[i] = true
			out = append(out, sessionLine(runs[i], i+1, depth, n == len(list)-1))
			walk(runs[i].Project, depth+1)
		}
	}
	walk(root, 1)
	// A cycle in engaged_by must not swallow a session.
	for i := range runs {
		if !seen[i] {
			out = append(out, sessionLine(runs[i], i+1, 1, true))
		}
	}
	return out
}

func sessionLine(g agentRun, num, depth int, last bool) string {
	branch := "├"
	if last {
		branch = "└"
	}
	line := fmt.Sprintf("%s%s %d. %s  %s  %s", strings.Repeat("  ", depth+1), branch,
		num, statusLabel(g.Status), g.Project, styDim.Render(g.Duration()))
	// The opencode session id, not the hmf name: this is the one `opencode -s`
	// takes, so the tree doubles as a way back in.
	if g.SessionID != "" {
		line += "  " + styDim.Render(g.SessionID)
	} else if g.Name != "" && g.Name != "-" {
		line += styDim.Render("  " + g.Name)
	}
	if g.Runs > 1 {
		note := fmt.Sprintf("  ·%d runs", g.Runs)
		if g.Failed > 0 {
			note += fmt.Sprintf(", %d failed", g.Failed)
		}
		line += styAttn.Render(note)
	}
	if g.Status == "active" && g.PID > 0 {
		line += styDim.Render(fmt.Sprintf("  pid %d", g.PID))
	}
	return line
}

// ============================================
// Detail
// ============================================

func (m monitorModel) detailPane() string {
	if m.detail < 0 || m.detail >= len(m.rows) {
		return styDim.Render("select a task")
	}
	_, rw := m.paneWidths()
	head := m.detailHead(m.rows[m.detail], rw-4)
	if !m.vpReady {
		return head + "\n" + m.detailBody()
	}
	return head + "\n" + m.vp.View()
}

// detailHead is the fixed part of the detail pane: identity, status and
// progress stay put while the body scrolls under them.
func (m monitorModel) detailHead(r monitorRow, w int) string {
	mark, sty := statusMark(r.Status)
	title := fmt.Sprintf("#%d  %s → %s", r.ThreadID, r.FromLabel(), r.ProjectLabel())
	status := sty.Render(mark + " " + statusWord(r.Status))
	title = truncate(title, max(8, w-lipgloss.Width(status)-2))
	gap := max(1, w-lipgloss.Width(title)-lipgloss.Width(status))
	line1 := styTitle.Render(title) + strings.Repeat(" ", gap) + status

	counts := "no work items"
	if r.TodosTotal > 0 {
		counts = fmt.Sprintf("%d/%d done", r.TodosDone, r.TodosTotal)
	}
	line2 := progressBar(r.TodosDone, r.TodosTotal, 12) + "  " +
		styDim.Render(counts+" · "+r.WorkElapsed())

	return line1 + "\n" + line2 + "\n" + styRule.Render(strings.Repeat("─", max(4, w)))
}

// detailBody is everything known about one task — its work items and the full
// back-and-forth on its thread.
func (m monitorModel) detailBody() string {
	if m.detail < 0 || m.detail >= len(m.rows) {
		return ""
	}
	r := m.rows[m.detail]
	var b strings.Builder
	width := m.bodyWidth()

	if r.Task != "" {
		b.WriteString(styLabel.Render("task") + "\n")
		for _, line := range wrapText(r.Task, width-2) {
			b.WriteString("  " + styText.Render(line) + "\n")
		}
		b.WriteString("\n")
	}

	// Who actually ran: the dispatcher at the root, each spawned session
	// under whoever engaged it. A task handed on (A engages B, B engages C)
	// shares one thread, so without the engaged-by edge the chain reads as a
	// flat list of unrelated attempts.
	if runs := groupRuns(r.Attempts); len(runs) > 0 {
		b.WriteString(styLabel.Render(fmt.Sprintf("sessions · %d", len(runs))) + "\n")
		b.WriteString("  " + styKey.Render(shortIdentity(r.From)) + styDim.Render("  (dispatcher)") + "\n")
		for _, line := range sessionTree(runs, shortIdentity(r.From)) {
			b.WriteString(line + "\n")
		}
		if a := firstResumable(r.Attempts); a != nil {
			b.WriteString("\n  " + styDim.Render("open one:  cd "+a.Dir+" && opencode -s <session id>") + "\n")
		}
		b.WriteString("\n")
	}

	if r.TodosTotal == 0 {
		b.WriteString(styDim.Render("no work items posted yet") + "\n\n")
	} else {
		head := fmt.Sprintf("work items · %d/%d done", r.TodosDone, r.TodosTotal)
		b.WriteString(styLabel.Render(head))
		if m.pickTodo {
			b.WriteString("  " + styKey.Render("↑↓ select · d delete · esc cancel"))
		}
		b.WriteString("\n")
		current := r.Current() // only the item being worked on gets the marker
		for i, t := range r.Todos {
			mark, text := styRule.Render("○"), styDim.Render(t.Content)
			if t.State == "done" {
				mark, text = styWorking.Render("✓"), styDim.Render(t.Content)
			} else if r.Status == "active" && t.Content == current {
				mark, text = styKey.Render("▸"), styText.Render(t.Content)
			}
			cursor := "  "
			if m.pickTodo && i == m.todoIdx {
				cursor, text = stySel.Render("▎")+" ", styText.Render(t.Content)
			}
			b.WriteString(cursor + mark + " " + text + "\n")
		}
		if m.confirmDel && m.todoIdx < len(r.Todos) {
			b.WriteString("\n  " + styFailed.Render("delete this work item?") + styDim.Render("  y / esc") + "\n")
			b.WriteString("  " + styDim.Render(r.Todos[m.todoIdx].Content) + "\n")
		}
		if m.notice != "" {
			b.WriteString("\n  " + styAttn.Render(m.notice) + "\n")
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
			when = styDim.Render(t.Local().Format("15:04") + " ")
		}
		var head, indent string
		if e.Dispatch {
			head = when + styKey.Render(e.Who()) + styDim.Render(" → "+e.To)
			indent = "  "
		} else {
			tag := ""
			switch e.Status {
			case "done":
				tag = " " + styDone.Render("done")
			case "ack":
				// Harness-generated, not the agent talking — label it so a
				// pickup notice is never mistaken for the child's own reply.
				tag = " " + styDim.Render("hmf")
			case "progress":
				tag = " " + styAttn.Render("progress")
			}
			head = when + styWorking.Render(e.Who()) + styDim.Render(" ↩") + tag
			indent = "      "
		}
		b.WriteString("\n" + indent + head + "\n")
		for _, line := range wrapText(e.Content, max(20, width-len(indent)-2)) {
			b.WriteString(indent + "  " + styText.Render(line) + "\n")
		}
	}
	return b.String()
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
		EngagedBy  string `json:"engaged_by"`
		PID        int    `json:"pid"`
		SessionID  string `json:"session_id"`
		Dir        string `json:"dir"`
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
			EngagedBy: s.EngagedBy, PID: s.PID,
			SessionID: s.SessionID, Dir: s.Dir,
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
		ID      int64  `json:"id"`
		Content string `json:"content"`
		State   string `json:"state"`
	}
	if json.Unmarshal(r, &td) != nil {
		return nil
	}
	out := make([]todoItem, 0, len(td))
	for _, t := range td {
		out = append(out, todoItem{ID: t.ID, Content: t.Content, State: t.State})
	}
	return out
}

func deleteTodo(id int64) error {
	_, err := protocol.Call("todo_delete", map[string]any{"id": id})
	return err
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
// WorkElapsed is time actually spent working: the sum of each attempt's own
// duration. Spanning first-start to now instead would bill the idle gap
// between a finished attempt and a much later follow-up as work — on a task
// picked up hours later that reads as 3h of effort for 30min of it.
func (r monitorRow) WorkElapsed() string {
	if len(r.Attempts) == 0 {
		return elapsed(r.CreatedAt, r.FinishedAt, r.Status == "active")
	}
	var total time.Duration
	unknown := false
	for _, a := range r.Attempts {
		start, ok := parseDaemonTime(a.CreatedAt)
		if !ok {
			unknown = true
			continue
		}
		switch end, ok := parseDaemonTime(a.FinishedAt); {
		case ok:
			total += end.Sub(start)
		case a.Status == "active":
			total += time.Since(start)
		default:
			unknown = true
		}
	}
	if total == 0 && unknown {
		return "?"
	}
	if unknown {
		return humanDuration(total) + "+?"
	}
	return humanDuration(total)
}

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
			r.WorkElapsed(),
			todos, truncate(firstLine(r.Task), 60))
	}
	return nil
}
