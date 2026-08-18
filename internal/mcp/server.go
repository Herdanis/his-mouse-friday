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

	"github.com/herdanis/his-mouse-friday/internal/config"
	"github.com/herdanis/his-mouse-friday/internal/protocol"
	mcpserver "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ============================================
// Tool input / output types
// ============================================

type PostInput struct {
	Channel  int64  `json:"channel,omitempty" jsonschema:"channel id (defaults to the global general channel)"`
	ThreadID int64  `json:"thread_id,omitempty" jsonschema:"thread id for replies; omit (0) for a thread root / new task"`
	To       string `json:"to,omitempty" jsonschema:"recipient workspace/project — a thread root with a to wakes that agent"`
	Content  string `json:"content" jsonschema:"message content"`
	Status   string `json:"status,omitempty" jsonschema:"delivered | in_progress | done | message (default)"`
}
type ReadChanInput struct {
	Channel int64 `json:"channel,omitempty" jsonschema:"channel id (defaults to the global general channel)"`
}
type ReadThreadInput struct {
	ThreadID int64 `json:"thread_id" jsonschema:"thread id"`
}
type ListInput struct {
	Workspace string `json:"workspace,omitempty" jsonschema:"workspace name filter"`
}

type MessageOutput struct {
	ID          int64  `json:"id"`
	ChannelID   int64  `json:"channel_id"`
	ThreadID    int64  `json:"thread_id"`
	FromProject string `json:"from_project"`
	ToProject   string `json:"to_project"`
	Content     string `json:"content"`
	Status      string `json:"status"`
	TS          string `json:"ts"`
}

type MessagesOutput struct {
	Messages []MessageOutput `json:"messages"`
}

type ProjectAgentOutput struct {
	Workspace string `json:"workspace"`
	Name      string `json:"name"`
	Path      string `json:"path"`
}

type ProjectAgentsOutput struct {
	Agents []ProjectAgentOutput `json:"agents"`
}

type PostOutput struct {
	MessageID int64 `json:"message_id" jsonschema:"the id of the posted message (use as thread_id for replies / task_status)"`
}
type TaskStatusInput struct {
	ThreadID int64 `json:"thread_id" jsonschema:"the task message id (thread root) to check status of"`
}
type TaskStatusOutput struct {
	HasDone     bool   `json:"has_done"`
	AgentStatus string `json:"agent_status" jsonschema:"working | exited | failed | no_agent"`
	SessionID   int64  `json:"session_id,omitempty"`
	PID         int    `json:"pid,omitempty"`
	ExitCode    int    `json:"exit_code,omitempty"`
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

// envChannelID reads HMF_CHANNEL_ID (set when spawned by hmf engage).
// Returns 0 if not set (user-initiated session).
func envChannelID() int64 {
	var id int64
	fmt.Sscanf(os.Getenv("HMF_CHANNEL_ID"), "%d", &id)
	return id
}

// ============================================
// MCP server
// ============================================

// newServer builds the MCP server and registers the 5 orchestration tools.
// callerID is the resolved "workspace/project" of the repo this shim runs in
// ("" if unregistered/open mode). It's injected as the `from` field on post
// calls so the daemon records who sent each message. A thread-root post
// (no thread_id) with a `to` wakes the addressed agent.
func newServer(callerID string) *mcpserver.Server {
	srv := mcpserver.NewServer(&mcpserver.Implementation{Name: "hmf-mcp", Version: "v0.1.0"}, nil)

	mcpserver.AddTool(srv, &mcpserver.Tool{
		Name:        "post_message",
		Description: "Post a message to the general channel (or a thread). A thread-root message (no thread_id) with a `to` wakes that agent. Replies set thread_id. Returns message_id.",
	}, func(ctx context.Context, req *mcpserver.CallToolRequest, in PostInput) (*mcpserver.CallToolResult, PostOutput, error) {
		params := map[string]any{
			"from":    callerID,
			"to":      in.To,
			"content": in.Content,
			"status":  in.Status,
		}
		if in.Channel != 0 {
			params["channel"] = in.Channel
		}
		if in.ThreadID != 0 {
			params["thread_id"] = in.ThreadID
		}
		result, err := callDaemon(ctx, "post_message", params)
		if err != nil {
			return nil, PostOutput{}, err
		}
		var pr PostOutput
		json.Unmarshal(result, &pr)
		return nil, pr, nil
	})

	mcpserver.AddTool(srv, &mcpserver.Tool{
		Name:        "task_status",
		Description: "Check the status of a delegated task: is the agent still working, exited cleanly, failed, or never woke. Plus whether a done reply has landed.",
	}, func(ctx context.Context, req *mcpserver.CallToolRequest, in TaskStatusInput) (*mcpserver.CallToolResult, TaskStatusOutput, error) {
		result, err := callDaemon(ctx, "task_status", map[string]any{"thread_id": in.ThreadID})
		if err != nil {
			return nil, TaskStatusOutput{}, err
		}
		var ts TaskStatusOutput
		if err := json.Unmarshal(result, &ts); err != nil {
			return nil, TaskStatusOutput{}, fmt.Errorf("decode task_status: %w", err)
		}
		return nil, ts, nil
	})

	mcpserver.AddTool(srv, &mcpserver.Tool{
		Name:        "read_channel",
		Description: "Read messages in a channel (defaults to this session's channel, else the global general channel)",
	}, func(ctx context.Context, req *mcpserver.CallToolRequest, in ReadChanInput) (*mcpserver.CallToolResult, MessagesOutput, error) {
		ch := in.Channel
		if ch == 0 {
			ch = envChannelID()
		}
		params := map[string]any{}
		if ch != 0 {
			params["channel"] = ch
		}
		result, err := callDaemon(ctx, "read_channel", params)
		if err != nil {
			return nil, MessagesOutput{}, err
		}
		var msgs []MessageOutput
		if err := json.Unmarshal(result, &msgs); err != nil {
			return nil, MessagesOutput{}, fmt.Errorf("decode messages: %w", err)
		}
		return nil, MessagesOutput{Messages: msgs}, nil
	})

	mcpserver.AddTool(srv, &mcpserver.Tool{
		Name:        "read_thread",
		Description: "Read a message thread",
	}, func(ctx context.Context, req *mcpserver.CallToolRequest, in ReadThreadInput) (*mcpserver.CallToolResult, MessagesOutput, error) {
		result, err := callDaemon(ctx, "read_thread", in)
		if err != nil {
			return nil, MessagesOutput{}, err
		}
		var msgs []MessageOutput
		if err := json.Unmarshal(result, &msgs); err != nil {
			return nil, MessagesOutput{}, fmt.Errorf("decode messages: %w", err)
		}
		return nil, MessagesOutput{Messages: msgs}, nil
	})

	mcpserver.AddTool(srv, &mcpserver.Tool{
		Name:        "list_project_agents",
		Description: "List registered project agents",
	}, func(ctx context.Context, req *mcpserver.CallToolRequest, in ListInput) (*mcpserver.CallToolResult, ProjectAgentsOutput, error) {
		result, err := callDaemon(ctx, "list_project_agents", in)
		if err != nil {
			return nil, ProjectAgentsOutput{}, err
		}
		var agents []ProjectAgentOutput
		if err := json.Unmarshal(result, &agents); err != nil {
			return nil, ProjectAgentsOutput{}, fmt.Errorf("decode agents: %w", err)
		}
		return nil, ProjectAgentsOutput{Agents: agents}, nil
	})

	return srv
}

// RunServer resolves the caller's identity, then starts the hmf-mcp server.
// If HMF_CHANNEL_ID is set (spawned by hmf engage), the caller is an engaged
// agent — its identity comes from HMF_PROJECT env, and post_message auto-fills
// the channel + from fields.
func RunServer(ctx context.Context) error {
	ensureDaemon(ctx)
	callerID := os.Getenv("HMF_PROJECT")
	if callerID == "" {
		repo, _ := os.Getwd()
		callerID = resolveCaller(repo)
	}
	if callerID != "" {
		fmt.Fprintf(os.Stderr, "hmf: ready (caller=%s)\n", callerID)
	} else {
		// Unregistered: check for global default mouse.yaml.
		cfg, _ := config.ResolveMouse(os.Getenv("PWD"))
		if cfg != nil {
			fmt.Fprintln(os.Stderr, "hmf: ready (unregistered — global default rules apply)")
		} else {
			fmt.Fprintln(os.Stderr, "hmf: ready (unregistered — open mode)")
		}
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
