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
	ParentID int64  `json:"root_id,omitempty" jsonschema:"set by hmf-mcp from HMF_TASK_MSG_ID when caller is a spawned agent — leave empty in user-initiated sessions"`
}
type ReadChanInput struct {
	Channel int64 `json:"channel,omitempty" jsonschema:"channel id (defaults to the global general channel)"`
}
type ReadThreadInput struct {
	MessageID int64 `json:"message_id" jsonschema:"any message id on the thread (root or reply) — the handler resolves it to the thread root"`
}
type ListInput struct {
	Workspace string `json:"workspace,omitempty" jsonschema:"workspace name filter"`
}

type TodoAddInput struct {
	ThreadID int64  `json:"thread_id" jsonschema:"thread root message id — the task this todo belongs to"`
	Content  string `json:"content" jsonschema:"one concrete work item"`
}
type TodoUpdateInput struct {
	ID    int64  `json:"id" jsonschema:"todo id"`
	State string `json:"state" jsonschema:"pending | done"`
}
type TodoListInput struct {
	ThreadID int64 `json:"thread_id" jsonschema:"thread root message id"`
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

type TodoOutput struct {
	ID        int64  `json:"id"`
	ThreadID  int64  `json:"thread_id"`
	Content   string `json:"content"`
	State     string `json:"state"`
	UpdatedAt string `json:"updated_at"`
}
type TodoAddOutput struct {
	ID int64 `json:"id" jsonschema:"the created todo id (use for todo_update)"`
}
type TodosOutput struct {
	Todos []TodoOutput `json:"todos"`
}

type PostOutput struct {
	MessageID int64 `json:"message_id" jsonschema:"the id of the posted message (use as thread_id for replies / task_status)"`
}
type TaskStatusInput struct {
	MessageID   int64 `json:"message_id" jsonschema:"any message id on the thread (root or reply) — the handler resolves it to the thread root"`
	WaitSeconds int64 `json:"wait_seconds,omitempty" jsonschema:"optional: block server-side up to this many seconds (capped at 120) until has_done or a terminal agent_status, instead of an instant snapshot. If has_done is still false, call again — that's the normal poll loop, no sleep needed."`
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

// newServer builds the MCP server + registers the 5 tools. callerID is the
// resolved "workspace/project" of this shim's repo ("" = open mode), injected
// as `from` on posts. Thread tracking: auto-binds every post_message to one
// thread per session — first post creates the root, subsequent posts inherit
// it; spawned agents inherit HMF_TASK_MSG_ID as their thread.
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
		// Auto-bind thread_id to the session's current thread if caller didn't set it.
		threadID := in.ThreadID
		if threadID == 0 {
			threadID = currentThread
		}
		if threadID != 0 {
			params["thread_id"] = threadID
		}
		// Spawned agent: propagate HMF_TASK_MSG_ID so cross-project delegations
		// bind to the original thread root.
		if tid := os.Getenv("HMF_TASK_MSG_ID"); tid != "" {
			var parentID int64
			fmt.Sscanf(tid, "%d", &parentID)
			if parentID != 0 {
				params["parent_id"] = parentID
			}
		}
		result, err := protocol.Call("post_message", params)
		if err != nil {
			return nil, PostOutput{}, err
		}
		var pr PostOutput
		json.Unmarshal(result, &pr)
		// First post (no prior thread) → bind session to this thread root.
		if currentThread == 0 && pr.MessageID != 0 {
			currentThread = pr.MessageID
		}
		return nil, pr, nil
	})

	mcpserver.AddTool(srv, &mcpserver.Tool{
		Name:        "task_status",
		Description: "Check the status of a delegated task: is the agent still working, exited cleanly, failed, or never woke. Plus whether a done reply has landed. Pass any message_id from the thread (root or reply) — the handler resolves it to the thread root. Pass wait_seconds to block until done instead of polling yourself.",
	}, func(ctx context.Context, req *mcpserver.CallToolRequest, in TaskStatusInput) (*mcpserver.CallToolResult, TaskStatusOutput, error) {
		// Deadline must outlive the daemon's own wait; cap mirrors daemon.maxTaskStatusWait.
		timeout := 30 * time.Second
		if in.WaitSeconds > 0 {
			timeout = min(time.Duration(in.WaitSeconds)*time.Second, 2*time.Minute) + 10*time.Second
		}
		result, err := protocol.CallWithTimeout("task_status", map[string]any{
			"message_id": in.MessageID, "wait_seconds": in.WaitSeconds,
		}, timeout)
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
		Description: "Read a message thread. Pass any message_id from the thread (root or reply) — the handler resolves it to the thread root and returns all messages in the conversation.",
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

	mcpserver.AddTool(srv, &mcpserver.Tool{
		Name:        "todo_add",
		Description: "Add a work item (todo) to a task thread. One todo = one concrete step. thread_id is the thread root message_id.",
	}, func(ctx context.Context, req *mcpserver.CallToolRequest, in TodoAddInput) (*mcpserver.CallToolResult, TodoAddOutput, error) {
		result, err := protocol.Call("todo_add", in)
		if err != nil {
			return nil, TodoAddOutput{}, err
		}
		var out TodoAddOutput
		if err := json.Unmarshal(result, &out); err != nil {
			return nil, TodoAddOutput{}, fmt.Errorf("decode todo_add: %w", err)
		}
		return nil, out, nil
	})

	mcpserver.AddTool(srv, &mcpserver.Tool{
		Name:        "todo_update",
		Description: "Mark a todo pending or done by id.",
	}, func(ctx context.Context, req *mcpserver.CallToolRequest, in TodoUpdateInput) (*mcpserver.CallToolResult, struct{}, error) {
		if _, err := protocol.Call("todo_update", in); err != nil {
			return nil, struct{}{}, err
		}
		return nil, struct{}{}, nil
	})

	mcpserver.AddTool(srv, &mcpserver.Tool{
		Name:        "todo_list",
		Description: "List all todos for a thread (pass the thread root message_id).",
	}, func(ctx context.Context, req *mcpserver.CallToolRequest, in TodoListInput) (*mcpserver.CallToolResult, TodosOutput, error) {
		result, err := protocol.Call("todo_list", in)
		if err != nil {
			return nil, TodosOutput{}, err
		}
		var todos []TodoOutput
		if err := json.Unmarshal(result, &todos); err != nil {
			return nil, TodosOutput{}, fmt.Errorf("decode todo_list: %w", err)
		}
		return nil, TodosOutput{Todos: todos}, nil
	})

	return srv
}

// RunServer resolves caller identity then starts the hmf-mcp server. If
// HMF_CHANNEL_ID is set, caller is a spawned agent (identity from HMF_PROJECT).
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

// ensureDaemon starts the daemon if the socket isn't reachable, so users
// can open opencode without running 'hmf up' first.
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
	// Plain Command (not CommandContext): the daemon must outlive this shim —
	// other opencode sessions may share it. Release: shim never Waits the child.
	cmd := exec.Command(bin, "up")
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.Stdin = nil
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "hmf: failed to start daemon: %v\n", err)
		return
	}
	_ = cmd.Process.Release()
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
