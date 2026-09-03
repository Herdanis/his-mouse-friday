package cli

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// A finished task must report how long it ran, not how long ago it started —
// otherwise its elapsed keeps climbing forever and reads as still-running.
func TestElapsed_FreezesWhenFinished(t *testing.T) {
	const layout = "2006-01-02 15:04:05.999999999 -0700 MST"
	start := time.Now().Add(-3 * time.Hour)
	finish := start.Add(90 * time.Second)

	got := elapsed(start.Format(layout), finish.Format(layout), false)
	if got != "1m30s" {
		t.Errorf("finished task: got %q, want 1m30s (its real duration)", got)
	}

	// Still running: counts up from the start.
	live := elapsed(time.Now().Add(-45*time.Second).Format(layout), "", true)
	if !strings.HasSuffix(live, "s") || live == "0s" {
		t.Errorf("running task: got %q, want a live count-up", live)
	}

	if got := elapsed("", "", true); got != "-" {
		t.Errorf("unparseable start: got %q, want -", got)
	}

	// Ended, but no finish time recorded and nothing to backfill from: the
	// duration is unknown, so don't invent a growing number.
	if got := elapsed(start.Format(layout), "", false); got != "?" {
		t.Errorf("finished with unknown duration: got %q, want ?", got)
	}
}

func TestHumanDuration(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{5 * time.Second, "5s"},
		{90 * time.Second, "1m30s"},
		{2*time.Hour + 5*time.Minute, "2h05m"},
		{-time.Second, "0s"},
	}
	for _, c := range cases {
		if got := humanDuration(c.d); got != c.want {
			t.Errorf("humanDuration(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

// TestWorkElapsed_ExcludesIdleGaps: a task picked up again hours later must
// report the work done, not the wall time spanning the wait. The old span
// calculation billed a 2h41m idle gap as effort.
func TestWorkElapsed_ExcludesIdleGaps(t *testing.T) {
	const layout = "2006-01-02 15:04:05.000000 -0700 MST"
	ts := func(s string) string {
		tm, err := time.Parse("15:04:05", s)
		if err != nil {
			t.Fatal(err)
		}
		return time.Date(2026, 9, 2, tm.Hour(), tm.Minute(), tm.Second(), 0, time.UTC).Format(layout)
	}
	r := monitorRow{
		CreatedAt: ts("08:21:55"),
		Status:    "exited",
		Attempts: []attempt{
			{Status: "exited", CreatedAt: ts("08:21:55"), FinishedAt: ts("08:53:19")}, // 31m24s
			{Status: "exited", CreatedAt: ts("11:34:36"), FinishedAt: ts("11:35:37")}, // 1m01s
		},
	}
	if got, want := r.WorkElapsed(), "32m25s"; got != want {
		t.Errorf("WorkElapsed() = %q, want %q (sum of attempts, not the 3h13m span)", got, want)
	}

	// An attempt that ended without a recorded finish is unknown, not zero.
	r.Attempts[1].FinishedAt = ""
	if got := r.WorkElapsed(); got != "31m24s+?" {
		t.Errorf("WorkElapsed() with an unknown attempt = %q, want 31m24s+?", got)
	}
}

// TestDetailBody_SessionTree: the detail view must show who dispatched the
// task and which sessions ran under it, nesting each agent under whoever
// engaged it so a hand-off reads as a chain, not as unrelated attempts.
func TestDetailBody_SessionTree(t *testing.T) {
	m := monitorModel{detail: 0, w: 100, rows: []monitorRow{{
		ThreadID: 7,
		From:     "haydn/his-mouse-friday",
		Status:   "active",
		Attempts: []attempt{
			{Name: "a1-penny", Project: "penny-pincher", Status: "exited",
				EngagedBy: "haydn/his-mouse-friday",
				SessionID: "ses_penny001", Dir: "/tmp/penny"},
			{Name: "a1-mouse", Project: "mouse-for-sale", Status: "active", PID: 42,
				EngagedBy: "haydn/penny-pincher",
				SessionID: "ses_mouse002", Dir: "/tmp/mouse"},
		},
	}}}
	out := m.detailBody()
	// The opencode session ids must be shown verbatim: they are what you paste
	// into `opencode -s` to reopen the session.
	for _, want := range []string{"sessions · 2", "(dispatcher)", "penny-pincher",
		"ses_penny001", "ses_mouse002", "pid 42", "mouse-for-sale", "opencode -s"} {
		if !strings.Contains(out, want) {
			t.Errorf("detail body missing %q\n%s", want, out)
		}
	}
	// The handed-off session sits deeper than the one the dispatcher engaged.
	direct := strings.Index(out, "1. ")
	nested := strings.Index(out, "2. ")
	if direct < 0 || nested < 0 {
		t.Fatalf("both sessions should be listed:\n%s", out)
	}
	if indentOf(out, nested) <= indentOf(out, direct) {
		t.Errorf("nested hand-off should be indented deeper:\n%s", out)
	}
}

// indentOf counts leading spaces on the line containing offset i.
func indentOf(s string, i int) int {
	start := strings.LastIndex(s[:i], "\n") + 1
	n := 0
	for _, r := range s[start:] {
		if r != ' ' {
			break
		}
		n++
	}
	return n
}

// TestGroupRuns_CollapsesResumedSession: resuming reuses the opencode session
// id, so repeated spawns are one child agent, not several. Collapsing them
// must keep the run count, any failure among them, and the summed time.
func TestGroupRuns_CollapsesResumedSession(t *testing.T) {
	const layout = "2006-01-02 15:04:05.000000 -0700 MST"
	at := func(h, m int) string {
		return time.Date(2026, 9, 2, h, m, 0, 0, time.UTC).Format(layout)
	}
	runs := groupRuns([]attempt{
		{Project: "mouse-for-sale", Status: "failed", SessionID: "ses_same",
			CreatedAt: at(10, 0), FinishedAt: at(10, 30)},
		{Project: "mouse-for-sale", Status: "exited", SessionID: "ses_same",
			CreatedAt: at(12, 0), FinishedAt: at(12, 15)},
	})
	if len(runs) != 1 {
		t.Fatalf("got %d entries, want 1 — same session id is one agent", len(runs))
	}
	g := runs[0]
	if g.Runs != 2 {
		t.Errorf("Runs = %d, want 2", g.Runs)
	}
	if g.Failed != 1 {
		t.Errorf("Failed = %d, want 1 — a failed run must survive collapsing", g.Failed)
	}
	if got := g.Duration(); got != "45m00s" {
		t.Errorf("Duration = %q, want 45m00s (summed across runs)", got)
	}
	if g.Status != "exited" {
		t.Errorf("Status = %q, want the latest run's status", g.Status)
	}
}

// Distinct agents must never be merged, including when no session id was
// captured — merging on a guess would hide a whole child.
func TestGroupRuns_KeepsDistinctAgentsApart(t *testing.T) {
	runs := groupRuns([]attempt{
		{Project: "penny-pincher", Status: "exited", SessionID: "ses_a"},
		{Project: "mouse-for-sale", Status: "exited", SessionID: "ses_b"},
		{Project: "ledger", Status: "exited", Name: "-"},
		{Project: "ledger", Status: "exited", Name: "-"},
	})
	if len(runs) != 4 {
		t.Fatalf("got %d entries, want 4 (two distinct ids + two unidentifiable)", len(runs))
	}
}

// TestListEntry_NamesTheDispatcher: the list column is the parent, not the
// worker. The parent is stable — present before anything spawns, unchanged
// when the task fans out to several agents — and the narrow layout has no
// detail pane to fall back on.
func TestListEntry_NamesTheDispatcher(t *testing.T) {
	m := monitorModel{w: 100, rows: []monitorRow{{
		ThreadID: 62,
		From:     "dir:ledger",
		Status:   "active",
		Attempts: []attempt{
			{Name: "a1-mouse", Project: "mouse-for-sale", Status: "active"},
		},
	}}}
	out := m.listEntry(0, 40)
	if !strings.Contains(out, "ledger/") {
		t.Errorf("list entry does not name the dispatcher\n%s", out)
	}
	if strings.Contains(out, "mouse-for-sale") {
		t.Errorf("list entry names the worker instead of the dispatcher\n%s", out)
	}
}

// A task nobody has spawned for yet still names its dispatcher.
func TestListEntry_DispatcherBeforeAnySpawn(t *testing.T) {
	m := monitorModel{w: 100, rows: []monitorRow{{
		ThreadID: 63, From: "haydn/his-mouse-friday", Status: "queued",
	}}}
	if out := m.listEntry(0, 40); !strings.Contains(out, "his-mouse-friday") {
		t.Errorf("list entry lost the dispatcher\n%s", out)
	}
}

// TestFooter_ConfirmsBeforeTaskDelete: D must never delete on the keystroke.
// The prompt lives in the footer because D works from the list too, where
// there is no detail pane to render into.
func TestFooter_ConfirmsBeforeTaskDelete(t *testing.T) {
	m := monitorModel{w: 100, rows: []monitorRow{{
		ThreadID: 62, From: "dir:ledger", Status: "exited",
	}}}
	if !strings.Contains(m.footer(), "del task") {
		t.Error("footer does not advertise the D binding")
	}
	m.confirmTask = true
	out := m.footer()
	for _, want := range []string{"#62", "ledger/", "y / esc"} {
		if !strings.Contains(out, want) {
			t.Errorf("confirm prompt missing %q\n%s", want, out)
		}
	}
}

// esc must clear the task confirm, or the next y deletes something the user
// already backed out of.
func TestEsc_ClearsTaskConfirm(t *testing.T) {
	m := monitorModel{w: 100, confirmTask: true, detail: -1, rows: []monitorRow{{ThreadID: 62}}}
	got, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if got.(monitorModel).confirmTask {
		t.Error("esc left the task confirm armed")
	}
}
