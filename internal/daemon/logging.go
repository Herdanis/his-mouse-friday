package daemon

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/herdanis/his-mouse-friday/internal/protocol"
)

// ============================================
// Event log
// ============================================

// Cap for the active file; at the cap it rotates to <path>.1 (one backup kept).
var logMaxBytes int64 = 40 << 20

var evlog = &eventLog{}

type eventLog struct {
	mu   sync.Mutex
	path string
	f    *os.File
	size int64
}

// LogPath is the active log file.
func LogPath() string { return filepath.Join(protocol.StateDir(), "hmf.log") }

// LogWriter exposes the log as an io.Writer (stdlib log, agent stdout/stderr).
func LogWriter() io.Writer { return evlog }

// Write never errors: a logging failure must not fail the caller.
func (l *eventLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.open(); err != nil {
		return len(p), nil
	}
	l.rotate(int64(len(p)))
	n, _ := l.f.Write(p)
	l.size += int64(n)
	return len(p), nil
}

// open lazily opens the file, reopening when StateDir changed (tests).
func (l *eventLog) open() error {
	path := LogPath()
	if l.f != nil && l.path == path {
		return nil
	}
	if l.f != nil {
		l.f.Close()
		l.f = nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0640)
	if err != nil {
		return err
	}
	l.f, l.path, l.size = f, path, 0
	if st, err := f.Stat(); err == nil {
		l.size = st.Size()
	}
	return nil
}

func (l *eventLog) rotate(incoming int64) {
	if l.size+incoming <= logMaxBytes {
		return
	}
	l.f.Close()
	l.f = nil
	if err := os.Rename(l.path, l.path+".1"); err != nil {
		os.Remove(l.path)
	}
	l.path = "" // force reopen
	_ = l.open()
}

// logf writes one timestamped event line.
func logf(event, format string, args ...any) {
	fmt.Fprintf(evlog, "%s [%s] %s\n",
		time.Now().Format("2006-01-02T15:04:05.000Z07:00"), event, fmt.Sprintf(format, args...))
}

// prefixWriter tags each write with a source label — used for agent output.
type prefixWriter struct{ prefix string }

func (w prefixWriter) Write(p []byte) (int, error) {
	logf(w.prefix, "%s", strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// trunc caps a logged value so one huge message can't dominate the file.
func trunc(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + fmt.Sprintf("...(%d bytes truncated)", len(s)-max)
}
