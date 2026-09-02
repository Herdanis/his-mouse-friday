package cli

import (
	"strings"
	"testing"
	"time"
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
