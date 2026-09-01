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
