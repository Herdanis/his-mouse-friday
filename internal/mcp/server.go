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
	RootID   int64  `json:"root_id,omitempty" jsonschema:"set by hmf-mcp from HMF_TASK_MSG_ID when caller is a spawned agent — leave empty in user-initiated sessions"`
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

// resolveCaller resolves the caller's workspace/project from a repo path via
// the daemon. Returns "" if unregistered (open mode — no enforcement).
func resolveCaller(repoPath string) string {
	result, err := protocol.Call("resolve_project",
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
// calls so the daemon records who sent each message.
//
// Thread tracking: the shim auto-binds every post_message to a single thread
// per session. Orchestrator sessions start with no thread — the first post
// creates a thread root, and the returned message_id becomes the session's
// thread. All subsequent posts auto-set thread_id=<that>. Spawned agents
// inherit HMF_TASK_MSG_ID as their thread (the root's). This means: one
// thread per root session, all agent-to-agent comms in the same conversation,
// new thread only when opencode restarts (new shim = new session).
func newServer(callerID string) *mcpserver.Server {
	srv := mcpserver.NewServer(&mcpserver.Implementation{Name: "hmf-mcp", Version: "v0.1.0"}, nil)

	// Thread binding: spawned agents inherit from env; orchestrator starts fresh.
	var currentThread int64
	if tid := os.Getenv("HMF_TASK_MSG_ID"); tid != "" {
		fmt.Sscanf(tid, "%d", &currentThread)
	}

	mcpserver.AddTool(srv, &mcpserver.Tool{
		Name:        "post_message",
		Description: "Post a message to the general channel (or a thread). A thread-root message (no thread_id) with a `to` wakes that agent. Replies set thread_id. Returns message_id. The shim auto-binds all your posts to the same thread per session — you don't need to manage thread_id manually. First post creates the thread; subsequent posts continue it (the agent resumes its prior session with context).",
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
		// Thread binding: if caller didn't set thread_id, auto-bind to the
		// session's current thread. First post has no current thread →
		// creates a thread root; the returned message_id becomes the
		// session's thread for all future posts.
		threadID := in.ThreadID
		if threadID == 0 {
			threadID = currentThread
		}
		if threadID != 0 {
			params["thread_id"] = threadID
		}
		// If caller is a spawned agent, propagate the caller's root so cross-
		// project delegations bind to the original thread root.
		if tid := os.Getenv("HMF_TASK_MSG_ID"); tid != "" {
			var rootID int64
			fmt.Sscanf(tid, "%d", &rootID)
			if rootID != 0 {
				params["root_id"] = rootID
			}
		}
		result, err := protocol.Call("post_message", params)
		if err != nil {
			return nil, PostOutput{}, err
		}
		var pr PostOutput
		json.Unmarshal(result, &pr)
		// First post (no prior thread) → bind the session to this thread root.
		if currentThread == 0 && pr.MessageID != 0 {
			currentThread = pr.MessageID
		}
		return nil, pr, nil
	})

	mcpserver.AddTool(srv, &mcpserver.Tool{
		Name:        "task_status",
		Description: "Check the status of a delegated task: is the agent still working, exited cleanly, failed, or never woke. Plus whether a done reply has landed.",
	}, func(ctx context.Context, req *mcpserver.CallToolRequest, in TaskStatusInput) (*mcpserver.CallToolResult, TaskStatusOutput, error) {
		result, err := protocol.Call("task_status", map[string]any{"thread_id": in.ThreadID})
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
		result, err := protocol.Call("read_channel", params)
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
		result, err := protocol.Call("read_thread", in)
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
		result, err := protocol.Call("list_project_agents", in)
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
