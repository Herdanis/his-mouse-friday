package protocol

import (
	"strings"
	"testing"
)

func TestSocketPath_EndsWithDaemonSock(t *testing.T) {
	got := SocketPath()
	if !strings.HasSuffix(got, ".hmf/daemon.sock") {
		t.Errorf("SocketPath() = %q, want suffix .hmf/daemon.sock", got)
	}
}

func TestDBPath_EndsWithHmfDb(t *testing.T) {
	got := DBPath()
	if !strings.HasSuffix(got, ".hmf/hmf.db") {
		t.Errorf("DBPath() = %q, want suffix .hmf/hmf.db", got)
	}
}
