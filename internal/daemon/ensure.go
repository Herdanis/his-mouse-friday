package daemon

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"time"

	"github.com/herdanis/his-mouse-friday/internal/protocol"
)

// EnsureRunning starts the daemon if the socket isn't reachable, so users can
// open the TUI or an opencode session without running 'hmf up' first.
// Idempotent: safe to call from every shim/TUI boot.
func EnsureRunning() error {
	if conn, err := net.Dial("unix", protocol.SocketPath()); err == nil {
		conn.Close()
		return nil
	}
	bin, err := exec.LookPath("hmf")
	if err != nil {
		return errors.New("hmf binary not found on PATH")
	}
	fmt.Fprintln(os.Stderr, "hmf: daemon down, starting...")
	// Plain Command (not CommandContext): the daemon must outlive this caller —
	// other sessions may share it. Release: caller never Waits the child.
	cmd := exec.Command(bin, "up")
	cmd.Stderr = LogWriter()
	cmd.Stdin = nil
	if err := cmd.Start(); err != nil {
		logErrf("ensure", "start daemon: %v", err)
		return fmt.Errorf("start daemon: %w", err)
	}
	_ = cmd.Process.Release()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if conn, err := net.Dial("unix", protocol.SocketPath()); err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	logErrf("ensure", "daemon did not become ready within 10s")
	return errors.New("daemon did not become ready within 10s")
}
