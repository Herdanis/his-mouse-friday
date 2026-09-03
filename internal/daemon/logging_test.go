package daemon

import (
	"os"
	"strings"
	"testing"
)

// Keep test event logs out of the real ~/.hmf.
func TestMain(m *testing.M) {
	if os.Getenv("HMF_STATE_DIR") == "" {
		dir, err := os.MkdirTemp("", "hmf-test-state")
		if err != nil {
			panic(err)
		}
		os.Setenv("HMF_STATE_DIR", dir)
	}
	code := m.Run()
	os.Exit(code)
}

func TestLogRotatesAtCap(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HMF_STATE_DIR", dir)
	old := logMaxBytes
	logMaxBytes = 200
	t.Cleanup(func() {
		logMaxBytes = old
		evlog.mu.Lock()
		if evlog.f != nil {
			evlog.f.Close()
			evlog.f, evlog.path = nil, ""
		}
		evlog.mu.Unlock()
	})

	for i := 0; i < 20; i++ {
		logf("test", "line %d %s", i, strings.Repeat("x", 50))
	}

	st, err := os.Stat(LogPath())
	if err != nil {
		t.Fatalf("no active log: %v", err)
	}
	if st.Size() > logMaxBytes {
		t.Fatalf("active log %d bytes exceeds cap %d", st.Size(), logMaxBytes)
	}
	if _, err := os.Stat(LogPath() + ".1"); err != nil {
		t.Fatalf("no rotated backup: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Fatalf("want 2 log files, got %d", len(entries))
	}
}
