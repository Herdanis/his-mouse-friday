// Package mcp runs the hmf-mcp shim: a stateless stdio MCP server that exposes
// 5 orchestration tools. Each tool call is forwarded to the hmf daemon over a
// unix socket and the daemon's response is relayed back to the caller.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"time"

	"github.com/herdanis/his-mouse-friday/internal/protocol"
	mcpserver "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ============================================
// Tool input / output types
// ============================================

type EngageInput struct {
	Project string `json:"project" jsonschema:"workspace/project to engage"`
	Task    string `json:"task" jsonschema:"task description"`
}
type EngageOutput struct {
	SessionID int64 `json:"session_id"`
	ChannelID int64 `json:"channel_id"`
}
type PostInput struct {
	Channel  int64  `json:"channel" jsonschema:"channel id"`
	ThreadID int64  `json:"thread_id,omitempty" jsonschema:"thread id for replies"`
	To       string `json:"to,omitempty" jsonschema:"recipient workspace/project"`
	Content  string `json:"content" jsonschema:"message content"`
}
type ReadChanInput struct {
	Channel int64 `json:"channel" jsonschema:"channel id"`
}
type ReadThreadInput struct {
	ThreadID int64 `json:"thread_id" jsonschema:"thread id"`
}
type ListInput struct {
	Workspace string `json:"workspace,omitempty" jsonschema:"workspace name filter"`
}

// ============================================
// Daemon client (unix socket)
// ============================================

func callDaemon(ctx context.Context, method string, params any) (json.RawMessage, error) {
	conn, err := net.Dial("unix", protocol.SocketPath())
	if err != nil {
		return nil, fmt.Errorf("daemon not running (run 'hmf up'): %w", err)
	}
	defer conn.Close()

	var raw json.RawMessage
	if params != nil {
		b, _ := json.Marshal(params)
		raw = b
	}
	req := protocol.Request{Method: method, Params: raw, ID: 1}

	enc := json.NewEncoder(conn)
	dec := json.NewDecoder(conn)
	if err := enc.Encode(&req); err != nil {
		return nil, err
	}
	var resp protocol.Response
	if err := dec.Decode(&resp); err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("%s", resp.Error.Message)
	}
	return resp.Result, nil
}

// resolveCaller resolves the caller's workspace/project from a repo path via
// the daemon. Returns "" if unregistered (open mode — no enforcement).
func resolveCaller(repoPath string) string {
	result, err := callDaemon(context.Background(), "resolve_project",
		map[string]string{"path": repoPath})
	if err != nil {
		return ""
	}
	var r struct {
		Workspace string `json:"workspace"`
		Project   string `json:"project"`
	}
	if json.Unmarshal(result, &r) != nil {
		return ""
	}
	if r.Workspace == "" {
		return ""
	}
	return r.Workspace + "/" + r.Project
}

// ============================================
// MCP server
// ============================================

// newServer builds the MCP server and registers the 5 orchestration tools.
// callerID is the resolved "workspace/project" of the repo this shim runs in
// ("" if unregistered/open mode). It's injected as the `from` field on engage
// and post calls so the daemon records who sent each message.
func newServer(callerID string) *mcpserver.Server {
	srv := mcpserver.NewServer(&mcpserver.Implementation{Name: "hmf-mcp", Version: "v0.1.0"}, nil)

	mcpserver.AddTool(srv, &mcpserver.Tool{
		Name:        "engage_project_agent",
		Description: "Spawn/resume target project agent and return session + channel id",
	}, func(ctx context.Context, req *mcpserver.CallToolRequest, in EngageInput) (*mcpserver.CallToolResult, EngageOutput, error) {
		params := map[string]any{
			"project": in.Project,
			"from":    callerID,
			"task":    in.Task,
		}
		result, err := callDaemon(ctx, "engage_project_agent", params)
		if err != nil {
			return nil, EngageOutput{}, err
		}
		var out EngageOutput
		if err := json.Unmarshal(result, &out); err != nil {
			return nil, EngageOutput{}, fmt.Errorf("decode engage result: %w", err)
		}
		return nil, out, nil
	})

	mcpserver.AddTool(srv, &mcpserver.Tool{
		Name:        "post_message",
		Description: "Post a message to a channel or thread",
	}, func(ctx context.Context, req *mcpserver.CallToolRequest, in PostInput) (*mcpserver.CallToolResult, struct{}, error) {
		params := map[string]any{
			"channel":  in.Channel,
			"from":     callerID,
			"to":       in.To,
			"content":  in.Content,
		}
		if in.ThreadID != 0 {
			params["thread_id"] = in.ThreadID
		}
		if _, err := callDaemon(ctx, "post_message", params); err != nil {
			return nil, struct{}{}, err
		}
		return nil, struct{}{}, nil
	})

	mcpserver.AddTool(srv, &mcpserver.Tool{
		Name:        "read_channel",
		Description: "Read messages in a channel",
	}, func(ctx context.Context, req *mcpserver.CallToolRequest, in ReadChanInput) (*mcpserver.CallToolResult, json.RawMessage, error) {
		result, err := callDaemon(ctx, "read_channel", in)
		if err != nil {
			return nil, nil, err
		}
		return nil, result, nil
	})

	mcpserver.AddTool(srv, &mcpserver.Tool{
		Name:        "read_thread",
		Description: "Read a message thread",
	}, func(ctx context.Context, req *mcpserver.CallToolRequest, in ReadThreadInput) (*mcpserver.CallToolResult, json.RawMessage, error) {
		result, err := callDaemon(ctx, "read_thread", in)
		if err != nil {
			return nil, nil, err
		}
		return nil, result, nil
	})

	mcpserver.AddTool(srv, &mcpserver.Tool{
		Name:        "list_project_agents",
		Description: "List registered project agents",
	}, func(ctx context.Context, req *mcpserver.CallToolRequest, in ListInput) (*mcpserver.CallToolResult, json.RawMessage, error) {
		result, err := callDaemon(ctx, "list_project_agents", in)
		if err != nil {
			return nil, nil, err
		}
		return nil, result, nil
	})

	return srv
}

// RunServer resolves the caller's identity from the repo path (cwd), then
// starts the hmf-mcp server over stdio and blocks until the context is
// cancelled or the transport closes.
func RunServer(ctx context.Context) error {
	ensureDaemon(ctx)
	repo, _ := os.Getwd()
	callerID := resolveCaller(repo)
	if callerID != "" {
		fmt.Fprintf(os.Stderr, "hmf: ready (caller=%s)\n", callerID)
	} else {
		fmt.Fprintln(os.Stderr, "hmf: ready (unregistered repo — open mode)")
	}
	return newServer(callerID).Run(ctx, &mcpserver.StdioTransport{})
}

// ensureDaemon checks if the daemon is reachable; if not, starts it in the
// background and waits for the socket to accept connections. This lets users
// open opencode without manually running 'hmf up' first.
func ensureDaemon(ctx context.Context) {
	if conn, err := net.Dial("unix", protocol.SocketPath()); err == nil {
		conn.Close()
		return
	}
	bin, err := exec.LookPath("hmf")
	if err != nil {
		fmt.Fprintln(os.Stderr, "hmf: 'hmf' binary not found on PATH — daemon auto-start skipped")
		return
	}
	fmt.Fprintln(os.Stderr, "hmf: daemon down, starting...")
	cmd := exec.CommandContext(ctx, bin, "up")
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.Stdin = nil
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "hmf: failed to start daemon: %v\n", err)
		return
	}
	// Wait up to 5s for the socket to accept.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if conn, err := net.Dial("unix", protocol.SocketPath()); err == nil {
			conn.Close()
			fmt.Fprintf(os.Stderr, "hmf: daemon started (pid %d)\n", cmd.Process.Pid)
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	fmt.Fprintln(os.Stderr, "hmf: daemon did not become ready within 5s")
}
