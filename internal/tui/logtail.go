package tui

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"strings"
)

// ============================================
// Log tail
// ============================================

// tailLog returns the last n lines of path containing the marker (e.g.
// "agent#7 " — trailing space, so agent#7 cannot match agent#70).
// errorsOnly keeps lines carrying the ERROR token. Missing file → empty
// slice, nil error (the daemon may not have logged yet). The log is capped
// at 40MB, so it is read backwards in 64KB chunks — never whole.
func tailLog(path, marker string, n int, errorsOnly bool) ([]string, error) {
	if n <= 0 {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}

	const chunkSize = 64 * 1024
	out := make([]string, 0, n)
	// pending holds the head of a line that started in a lower chunk and
	// continues past pos — rebuilt as chunks are consumed backwards.
	pending := ""
	emit := func(line string) bool {
		if line == "" || !strings.Contains(line, marker) {
			return false
		}
		if errorsOnly && !strings.Contains(line, "ERROR") {
			return false
		}
		out = append(out, line)
		return len(out) >= n
	}

	for pos := st.Size(); pos > 0 && len(out) < n; {
		start := max(pos-chunkSize, 0)
		buf := make([]byte, pos-start)
		if _, err := f.ReadAt(buf, start); err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		segs := strings.Split(string(buf), "\n")
		// segs[last] + pending is the line straddling the pos boundary. It is
		// complete only when this chunk holds a '\n' or reaches the file
		// start; a newline-free chunk belongs to a line continuing below —
		// carry it, never emit, or the same bytes come out twice.
		if len(segs) > 1 || start == 0 {
			if emit(segs[len(segs)-1] + pending) {
				break
			}
		}
		low := 0
		if start > 0 {
			// segs[0] has no leading newline in this chunk — it may be the
			// tail of a line continued from below; carry it down.
			low = 1
			pending = segs[0] + pending
		}
		done := false
		for i := len(segs) - 2; i >= low; i-- {
			if emit(segs[i]) {
				done = true
				break
			}
		}
		if done {
			break
		}
		pos = start
	}

	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}
