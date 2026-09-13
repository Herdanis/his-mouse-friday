package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTailLogFiltersByMarker(t *testing.T) {
	p := filepath.Join(t.TempDir(), "hmf.log")
	// Marker is "agent#7 " with a trailing space (real launcher prefix),
	// so agent#70 must not match.
	if err := os.WriteFile(p, []byte(strings.Join([]string{
		"[spawn] agent#7 ab000-child started",
		"[rpc] id=1 method=status",
		"[agent#7 ab000-child] reading files",
		"[agent#7 ab000-child] ERROR: tests failed",
		"[agent#70 other] decoy line",
		"[agent#8] other worker line",
	}, "\n")), 0644); err != nil {
		t.Fatal(err)
	}
	lines, err := tailLog(p, "agent#7 ", 10, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 3 || !strings.Contains(lines[2], "ERROR: tests failed") {
		t.Fatalf("got %v", lines)
	}
	errLines, _ := tailLog(p, "agent#7 ", 10, true)
	if len(errLines) != 1 || !strings.Contains(errLines[0], "ERROR") {
		t.Fatalf("errorsOnly: %v", errLines)
	}
	one, _ := tailLog(p, "agent#7 ", 1, false)
	if len(one) != 1 || !strings.Contains(one[0], "ERROR: tests failed") {
		t.Fatalf("n=1 must keep the newest match: %v", one)
	}
}

func TestTailLogMissingFile(t *testing.T) {
	lines, err := tailLog(filepath.Join(t.TempDir(), "nope.log"), "agent#7 ", 5, false)
	if err != nil || len(lines) != 0 {
		t.Fatalf("missing file must be empty slice, nil error; got %v, %v", lines, err)
	}
}

func TestTailLogAcrossChunks(t *testing.T) {
	p := filepath.Join(t.TempDir(), "hmf.log")
	var b strings.Builder
	for i := 0; i < 2000; i++ {
		if i%10 == 0 {
			fmt.Fprintf(&b, "agent#7 line %04d padded %s\n", i, strings.Repeat("x", 60))
		} else {
			fmt.Fprintf(&b, "filler %04d padded %s\n", i, strings.Repeat("x", 60))
		}
	}
	if err := os.WriteFile(p, []byte(b.String()), 0644); err != nil {
		t.Fatal(err)
	}
	lines, err := tailLog(p, "agent#7 ", 3, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 3 ||
		!strings.Contains(lines[0], "1970") ||
		!strings.Contains(lines[1], "1980") ||
		!strings.Contains(lines[2], "1990") {
		t.Fatalf("want last three matches oldest-first, got %v", lines)
	}
}
