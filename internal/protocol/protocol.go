package protocol

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

type Request struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
	ID     int64           `json:"id"`
}

type Response struct {
	ID     int64           `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *ResponseError  `json:"error,omitempty"`
}

type ResponseError struct {
	Message string `json:"message"`
}

// StateDir returns the daemon's state directory. Respects HMF_STATE_DIR for
// tests (temp dir) + future relocation; defaults to ~/.hmf.
func StateDir() string {
	if dir := os.Getenv("HMF_STATE_DIR"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".hmf")
}

// SocketPath returns the daemon's unix socket path.
func SocketPath() string { return filepath.Join(StateDir(), "daemon.sock") }

// DBPath returns the daemon's SQLite database path.
func DBPath() string { return filepath.Join(StateDir(), "hmf.db") }

// Call dials the daemon, sends one request, returns the decoded Result.
// Folds the resp.Error check into the returned error so callers don't repeat
// the `if resp.Error != nil` dance. Shared by the CLI and the MCP shim.
// 30s cap: every RPC except a blocking task_status is fast (spawn is async);
// a hung daemon must not block the CLI forever.
func Call(method string, params any) (json.RawMessage, error) {
	return CallWithTimeout(method, params, 30*time.Second)
}

// CallWithTimeout is Call with a caller-chosen deadline — for RPCs that may
// legitimately block server-side (task_status blocks up to 5min).
func CallWithTimeout(method string, params any, timeout time.Duration) (json.RawMessage, error) {
	var raw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return nil, fmt.Errorf("marshal params: %w", err)
		}
		raw = b
	}
	conn, err := net.Dial("unix", SocketPath())
	if err != nil {
		return nil, fmt.Errorf("daemon not running (run 'hmf up'): %w", err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return nil, fmt.Errorf("set deadline: %w", err)
	}
	enc := json.NewEncoder(conn)
	dec := json.NewDecoder(conn)
	if err := enc.Encode(&Request{Method: method, Params: raw, ID: 1}); err != nil {
		return nil, err
	}
	var resp Response
	if err := dec.Decode(&resp); err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("%s", resp.Error.Message)
	}
	return resp.Result, nil
}
