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
	"path/filepath"
	"strings"
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
type ReportProgressInput struct {
	Note       string `json:"note" jsonschema:"what you are doing right now, in one line — concrete, not 'still working'"`
	ETAMinutes int    `json:"eta_minutes,omitempty" jsonschema:"how many more minutes you expect to need; omit if you genuinely cannot tell"`
	ThreadID   int64  `json:"thread_id,omitempty" jsonschema:"leave empty when spawned by hmf — inherited from HMF_TASK_MSG_ID"`
}
type TaskStatusInput struct {
	MessageID int64 `json:"message_id" jsonschema:"any message id on the thread (root or reply) — the handler resolves it to the thread root"`
}
type TaskStatusOutput struct {
	HasDone     bool   `json:"has_done"`
	AgentStatus string `json:"agent_status" jsonschema:"working | exited | failed | no_agent"`
	SessionID   int64  `json:"session_id,omitempty"`
	PID         int    `json:"pid,omitempty"`
	ExitCode    int    `json:"exit_code,omitempty"`
	NextAction  string `json:"next_action" jsonschema:"what to do next — follow it literally. Anything other than STILL WORKING is final: stop calling task_status for this message_id."`

	Project     string     `json:"project,omitempty" jsonschema:"the project doing the work"`
	ElapsedSecs int64      `json:"elapsed_secs,omitempty" jsonschema:"seconds since the task was dispatched"`
	TodosDone   int        `json:"todos_done" jsonschema:"work items the child has finished"`
	TodosTotal  int        `json:"todos_total" jsonschema:"work items the child has planned (0 = it hasn't posted any yet)"`
	CurrentStep string     `json:"current_step,omitempty" jsonschema:"the work item it is on right now"`
	Todos       []TodoLine `json:"todos,omitempty" jsonschema:"the child's full work-item list with state"`
	LastUpdate  string     `json:"last_update,omitempty" jsonschema:"first line of the most recent reply on the thread"`

	ProgressNote    string `json:"progress_note,omitempty" jsonschema:"the child's own latest account of what it is doing"`
	ETAMinutes      int    `json:"eta_minutes,omitempty" jsonschema:"how much longer the child said it needs, from that same report"`
	ProgressAgeSecs int64  `json:"progress_age_secs,omitempty" jsonschema:"seconds since that report — read the ETA against this; a stale note means the child stopped narrating, not that it is on track"`
}

// TodoLine is one of the child's work items.
type TodoLine struct {
	Content string `json:"content"`
	State   string `json:"state" jsonschema:"pending | done"`
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

// dirIdentity names an unregistered caller by the directory it runs in. The
// "dir:" prefix and the absence of a slash keep it distinguishable from a
// real "workspace/project", which several code paths rely on.
func dirIdentity(repo string) string {
	base := filepath.Base(strings.TrimRight(repo, string(filepath.Separator)))
	if base == "" || base == "." || base == string(filepath.Separator) {
		return ""
	}
	return "dir:" + base
}

// envTaskMsgID reads HMF_TASK_MSG_ID — the thread a spawned agent works on.
func envTaskMsgID() int64 {
	var id int64
	fmt.Sscanf(os.Getenv("HMF_TASK_MSG_ID"), "%d", &id)
	return id
}

// envChannelID reads HMF_CHANNEL_ID (set when spawned by hmf engage).
// Returns 0 if not set (user-initiated session).
func envChannelID() int64 {
	var id int64
	fmt.Sscanf(os.Getenv("HMF_CHANNEL_ID"), "%d", &id)
	return id
}

// resolveThreadID picks the thread a post belongs to when the caller didn't
// name one: everything from a session joins that session's thread, so work
// split across several projects stays one parent task rather than becoming
// unrelated roots.
//
// This is safe only because the daemon's wake guard is scoped per project —
// a thread-wide guard would swallow the second project's spawn.
func resolveThreadID(explicit int64, to string, byRecipient map[string]int64, current int64) int64 {
	if explicit != 0 {
		return explicit
	}
	return current
}

// ============================================
// MCP server
// ============================================

// newServer builds the MCP server + registers the 6 tools. callerID is the
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
	// Per-recipient thread roots, so parallel delegations don't share a thread.
	threadByRecipient := map[string]int64{}

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
		threadID := resolveThreadID(in.ThreadID, in.To, threadByRecipient, currentThread)
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
		if pr.MessageID != 0 {
			// New root for this recipient → remember it so follow-ups continue
			// (and resume) the same thread instead of starting a fresh one.
			if in.To != "" && threadID == 0 {
				threadByRecipient[in.To] = pr.MessageID
			}
			// First post (no prior thread) → bind session to this thread root.
			if currentThread == 0 {
				currentThread = pr.MessageID
			}
		}
		return nil, pr, nil
	})

	mcpserver.AddTool(srv, &mcpserver.Tool{
		Name:        "report_progress",
		Description: "Tell the agent that delegated this task where you are, when the work is taking a while. Call it when you realise the job needs more time than a couple of minutes, whenever your estimate changes materially, and before any long stretch of quiet work. Say what you are doing now and how much longer you expect to need. The parent cannot see your session — without this it only knows you are alive, not what you are doing or whether to keep waiting. Costs the parent nothing: it never wakes anyone.",
	}, func(ctx context.Context, req *mcpserver.CallToolRequest, in ReportProgressInput) (*mcpserver.CallToolResult, PostOutput, error) {
		tid := in.ThreadID
		if tid == 0 {
			tid = envTaskMsgID()
		}
		if tid == 0 {
			return nil, PostOutput{}, fmt.Errorf("no thread to report on: pass thread_id (spawned agents inherit it from HMF_TASK_MSG_ID)")
		}
		result, err := protocol.Call("report_progress", map[string]any{
			"thread_id":   tid,
			"from":        os.Getenv("HMF_PROJECT"),
			"note":        in.Note,
			"eta_minutes": in.ETAMinutes,
		})
		if err != nil {
			return nil, PostOutput{}, err
		}
		var out PostOutput
		if err := json.Unmarshal(result, &out); err != nil {
			return nil, PostOutput{}, fmt.Errorf("decode report_progress: %w", err)
		}
		return nil, out, nil
	})

	mcpserver.AddTool(srv, &mcpserver.Tool{
		Name:        "task_status",
		Description: "Check on a delegated task and see what the child agent is actually doing: its work items (todos with state), the step it's on, how long it's been running, and its latest reply — plus whether it finished, exited, failed, or never woke. This is how you observe a child from here; it runs in its own process and cannot write into your session. Pass any message_id from the thread (root or reply). BLOCKS up to 5 minutes, returning early the moment the task finishes — do not sleep around it, and do not call it again until it returns. Follow the next_action field it gives you.",
	}, func(ctx context.Context, req *mcpserver.CallToolRequest, in TaskStatusInput) (*mcpserver.CallToolResult, TaskStatusOutput, error) {
		// Socket deadline must outlive the daemon's fixed 5min wait.
		result, err := protocol.CallWithTimeout("task_status",
			map[string]any{"message_id": in.MessageID}, 5*time.Minute+10*time.Second)
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
		if callerID == "" {
			// Unregistered directory: still say where the work came from, so
			// its threads are attributable instead of anonymous.
			callerID = dirIdentity(repo)
		}
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
