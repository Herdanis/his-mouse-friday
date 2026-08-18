package protocol

import (
	"encoding/json"
	"os"
	"path/filepath"
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
