package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/herdanis/his-mouse-friday/internal/config"
	"github.com/herdanis/his-mouse-friday/internal/protocol"
)

// ============================================
// Daemon
// ============================================

// Daemon owns all state and brokers agent-to-agent communication.
// Exported fields because Task 9 (CLI) and Task 11 (e2e) construct it directly
// from a different package.
type Daemon struct {
	Store       *Store
	Registry    *Registry
	Sessions    *SessionStore
	Comms       *Comms
	Launcher    *Launcher
	MouseLoader func(string) (*config.MouseConfig, error)
	// SafetyNetEnabled controls the OnExit synthetic BLOCKED reply. Default
	// true (production). Tests that use /bin/echo as the agent binary set
	// this false — /bin/echo exits immediately and would spuriously fire the
	// safety net, polluting threads the test then manually populates with
	// done replies.
	SafetyNetEnabled bool
	Sock             string
	shutdownCh       chan struct{}
}

// NewDaemon wires a Daemon with default components for the given socket + db.
func NewDaemon(sock, dbPath string) (*Daemon, error) {
	store, err := OpenStore(dbPath)
	if err != nil {
		return nil, err
	}
	return &Daemon{
		Store:            store,
		Registry:         &Registry{Store: store},
		Sessions:         &SessionStore{Store: store},
		Comms:            &Comms{Store: store},
		Launcher:         &Launcher{},
		MouseLoader:      config.LoadMouse,
		SafetyNetEnabled: true,
		Sock:             sock,
		shutdownCh:       make(chan struct{}),
	}, nil
}

// ============================================
// Wire params / results
// ============================================

type PostParams struct {
	Channel  int64  `json:"channel"`
	ThreadID int64  `json:"thread_id,omitempty"`
	From     string `json:"from"`
	To       string `json:"to"`
	Content  string `json:"content"`
	Status   string `json:"status,omitempty"` // delivered | in_progress | done | message (default)
	RootID   int64  `json:"root_id,omitempty"`
}
type PostResult struct {
	MessageID int64 `json:"message_id"`
}
type TaskStatusParams struct {
	ThreadID int64 `json:"thread_id"`
}
type TaskStatusResult struct {
	HasDone     bool   `json:"has_done"`
	AgentStatus string `json:"agent_status"` // working | exited | failed | no_agent
	SessionID   int64  `json:"session_id,omitempty"`
	PID         int    `json:"pid,omitempty"`
	ExitCode    int    `json:"exit_code,omitempty"`
}
type ReadChanParams struct {
	Channel int64 `json:"channel"`
}
type ReadThreadParams struct {
	ThreadID int64 `json:"thread_id"`
}
type WorkspaceAddParams struct {
	Name string `json:"name"`
}
type ProjectAddParams struct {
	Workspace string `json:"workspace"`
	Name      string `json:"name"`
	Path      string `json:"path"`
}
type ProjectAddResult struct {
	ID int64 `json:"id"`
}
type ResolveParams struct {
	Path string `json:"path"`
}
type ResolveResult struct {
	Workspace string `json:"workspace"`
	Project   string `json:"project"`
}
type ListParams struct {
	Workspace string `json:"workspace"`
}
type StatusResult struct {
	Running    bool   `json:"running"`
	Workspaces int    `json:"workspaces"`
	Projects   int    `json:"projects"`
	Sessions   int    `json:"sessions"`
	Sock       string `json:"sock"`
}

// ============================================
// Method dispatch
// ============================================

// Handle dispatches a single request.
func (d *Daemon) Handle(ctx context.Context, req protocol.Request) protocol.Response {
	switch req.Method {
	case "post_message":
		return d.handlePost(ctx, req)
	case "task_status":
		return d.handleTaskStatus(req)
	case "read_channel":
		return d.handleReadChannel(req)
	case "read_thread":
		return d.handleReadThread(req)
	case "list_project_agents":
		return d.handleProjectList(req)
	case "resolve_project":
		return d.handleResolve(req)
	case "workspace_add":
		return d.handleWorkspaceAdd(req)
	case "workspace_list":
		return d.handleWorkspaceList(req)
	case "workspace_delete":
		return d.handleWorkspaceDelete(req)
	case "project_add":
		return d.handleProjectAdd(req)
	case "project_list":
		return d.handleProjectList(req)
	case "project_delete":
		return d.handleProjectDelete(req)
	case "status":
		return d.handleStatus(req)
	case "shutdown":
		select {
		case <-d.shutdownCh:
			// already shutting down
		default:
			close(d.shutdownCh)
		}
		return protocol.Response{ID: req.ID, Result: json.RawMessage(`{}`)}
	default:
		return protocol.Response{ID: req.ID, Error: &protocol.ResponseError{Message: "unknown method: " + req.Method}}
	}
}

// findProject looks up a project by workspace name + project name.
func (d *Daemon) findProject(ws, name string) (Project, error) {
	var p Project
	err := d.Store.db.QueryRow(
		`SELECT id, workspace_id, name, path FROM projects WHERE name=? AND workspace_id=(SELECT id FROM workspaces WHERE name=?)`,
		name, ws).Scan(&p.ID, &p.WorkspaceID, &p.Name, &p.Path)
	return p, err
}

// threadHasDone reports whether any message on the thread (root or replies)
// has status='done'. Used to suppress re-waking a finished thread.
func (d *Daemon) threadHasDone(threadID int64) bool {
	if threadID == 0 {
		return false
	}
	var n int
	d.Store.db.QueryRow(
		`SELECT count(*) FROM messages WHERE (id=? OR thread_id=?) AND status='done'`,
		threadID, threadID).Scan(&n)
	return n > 0
}

// threadSessionActive reports whether the latest session bound to the thread
// is still active (process still running). Used to suppress a second wake
// while the first is still working.
//
// ponytail: queries root_thread_id (not task_msg_id) so it catches the
// reply-wake session too — Task 8's reply-wake stores task_msg_id=<reply-msg-id>,
// not the thread root, so a task_msg_id=? filter would miss it.
func (d *Daemon) threadSessionActive(threadID int64) bool {
	if threadID == 0 {
		return false
	}
	var status string
	err := d.Store.db.QueryRow(
		`SELECT status FROM sessions WHERE root_thread_id=? ORDER BY id DESC LIMIT 1`,
		threadID).Scan(&status)
	if err != nil {
		return false // no session → not active
	}
	return status == "active"
}

func (d *Daemon) handlePost(ctx context.Context, req protocol.Request) protocol.Response {
	var p PostParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return errResp(req.ID, "bad params: "+err.Error())
	}
	// Default channel = the global "general" (the lobby where all agents live).
	chID := p.Channel
	if chID == 0 {
		gen, err := d.Comms.GetOrCreateGeneralChannel()
		if err != nil {
			return errResp(req.ID, "general channel: "+err.Error())
		}
		chID = gen.ID
	}
	msg, err := d.Comms.PostMessage(chID, p.ThreadID, p.From, p.To, p.Content, p.Status)
	if err != nil {
		return errResp(req.ID, err.Error())
	}
	// Wake any to-addressed message — thread root OR reply. Two guards
	// suppress the wake: (1) the thread already has a done reply (finished
	// — don't re-wake), (2) the thread's latest session is still active
	// (agent running — let it pick up the new message on its next turn).
	if p.To != "" {
		if d.threadHasDone(p.ThreadID) {
			// Message still posts — visible in read_thread. Just no wake.
		} else if d.threadSessionActive(p.ThreadID) {
			// Agent still running — no new wake.
		} else {
			if err := d.wakeAgent(ctx, p, msg); err != nil {
				return errResp(req.ID, "wake: "+err.Error())
			}
		}
	}
	result, _ := json.Marshal(PostResult{MessageID: msg.ID})
	return protocol.Response{ID: req.ID, Result: result}
}

// wakeAgent spawns the project agent addressed by msg.ToProject so it can read
// the thread rooted at msg.ID and reply. Serverless: boot per mention, handle,
// reply in-thread, exit.
func (d *Daemon) wakeAgent(ctx context.Context, p PostParams, msg Message) error {
	parts := strings.SplitN(msg.ToProject, "/", 2)
	if len(parts) != 2 {
		return fmt.Errorf("to must be workspace/project, got %q", msg.ToProject)
	}
	proj, err := d.findProject(parts[0], parts[1])
	if err != nil {
		// Not a registered agent — post the message but don't wake. Mailbox
		// semantics: you can address anyone; delivery happens only for
		// registered agents. Lets post_message stay a plain insert when the
		// recipient isn't registered (backward compat + graceful addressing).
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("project %s not found: %w", msg.ToProject, err)
	}
	mouse, err := d.MouseLoader(filepath.Join(proj.Path, "mouse.yaml"))
	if err != nil {
		return fmt.Errorf("read mouse.yaml: %w", err)
	}
	if mouse != nil && !mouse.A2A.AllowInbound {
		return fmt.Errorf("project %s does not allow inbound engagement", msg.ToProject)
	}
	binary := "opencode"
	model := "default"
	if mouse != nil && mouse.Agent.Primary.Provider != "" {
		binary = mouse.Agent.Primary.Provider
		if mouse.Agent.Primary.Model != "" {
			model = mouse.Agent.Primary.Model
		}
	}
	runbook, _ := os.ReadFile(filepath.Join(proj.Path, "MOUSE.md"))
	// rootID: explicitly passed (cross-project delegation from a child agent
	// — root_id sourced from caller's HMF_TASK_MSG_ID). For a thread root,
	// fall back to msg.ID. For a reply wake, walk up to msg.ThreadID.
	rootID := msg.ID
	if p.RootID != 0 {
		rootID = p.RootID
	} else if msg.ThreadID != 0 {
		rootID = msg.ThreadID
	}
	tmpSess, err := d.Sessions.Create(proj.ID, binary, model, 0, msg.ID, rootID, "", "")
	if err != nil {
		return fmt.Errorf("session: %w", err)
	}
	entrypoint := fmt.Sprintf(
		"You were @mentioned in the general channel, message id %d. "+
			"Call the read_thread MCP tool with thread_id=%d to load the task and any prior context. "+
			"Handle the request. Reply in the thread using post_message with thread_id=%d, status=\"done\", "+
			"and a one-line summary of what you did.",
		msg.ID, msg.ID, msg.ID)
	pid, err := d.Launcher.Spawn(ctx, SpawnConfig{
		Dir:       proj.Path,
		Binary:    binary,
		Model:     model,
		Runbook:   string(runbook),
		Task:      entrypoint,
		FromID:    msg.FromProject,
		ProjectID: msg.ToProject,
		ChannelID: msg.ChannelID,
		SessionID: tmpSess.ID,
		TaskMsgID: msg.ID,
		OnExit: func(code int) {
			d.Sessions.MarkExited(tmpSess.ID, code)
			if !d.SafetyNetEnabled {
				return
			}
			// Safety net: if the agent exited without posting a done reply,
			// post a synthetic BLOCKED reply on its behalf so the orchestrator
			// stops polling and surfaces the failure instead of waiting
			// forever. Covers crashes, kills, and the common "agent hit
			// bash:ask, no TTY, exited silently" case the launcher prompt
			// can't fully solve (the spawned model may not follow the
			// MUST-REPLY rule).
			var doneCount int
			d.Store.db.QueryRow(
				`SELECT count(*) FROM messages WHERE thread_id=? AND status='done'`, msg.ID).Scan(&doneCount)
			if doneCount == 0 {
				content := fmt.Sprintf("BLOCKED: agent exited (code=%d) without posting a done reply", code)
				if _, err := d.Comms.PostMessage(msg.ChannelID, msg.ID, msg.ToProject, msg.FromProject, content, "done"); err != nil {
					log.Printf("synthetic done reply for thread %d: %v", msg.ID, err)
				}
			}
		},
	})
	if err != nil {
		d.Sessions.SetStatus(tmpSess.ID, "failed")
		return fmt.Errorf("spawn: %w", err)
	}
	d.Sessions.SetPID(tmpSess.ID, pid)
	d.Sessions.SetStatus(tmpSess.ID, "active")
	return nil
}

// handleTaskStatus reports the state of the agent spawned for a task thread:
// is it still working, exited cleanly, failed, or never woke. Plus whether a
// done reply has landed. Lets the orchestrator poll "still working vs died"
// instead of guessing from channel reads.
func (d *Daemon) handleTaskStatus(req protocol.Request) protocol.Response {
	var p TaskStatusParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return errResp(req.ID, "bad params: "+err.Error())
	}
	if p.ThreadID == 0 {
		return errResp(req.ID, "thread_id is required")
	}
	var doneCount int
	d.Store.db.QueryRow(
		`SELECT count(*) FROM messages WHERE thread_id=? AND status='done'`, p.ThreadID).Scan(&doneCount)

	var sessID int64
	var status string
	var pid sql.NullInt64
	var exitCode sql.NullInt64
	err := d.Store.db.QueryRow(
		`SELECT id, status, pid, exit_code FROM sessions WHERE task_msg_id=? ORDER BY id DESC LIMIT 1`,
		p.ThreadID).Scan(&sessID, &status, &pid, &exitCode)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			result, _ := json.Marshal(TaskStatusResult{HasDone: doneCount > 0, AgentStatus: "no_agent"})
			return protocol.Response{ID: req.ID, Result: result}
		}
		return errResp(req.ID, err.Error())
	}
	// "active" → "working"; other statuses pass through.
	agentStatus := status
	if status == "active" {
		agentStatus = "working"
	}
	result, _ := json.Marshal(TaskStatusResult{
		HasDone:     doneCount > 0,
		AgentStatus: agentStatus,
		SessionID:   sessID,
		PID:         int(pid.Int64),
		ExitCode:    int(exitCode.Int64),
	})
	return protocol.Response{ID: req.ID, Result: result}
}

func (d *Daemon) handleReadChannel(req protocol.Request) protocol.Response {
	var p ReadChanParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return errResp(req.ID, "bad params: "+err.Error())
	}
	chID := p.Channel
	if chID == 0 {
		gen, err := d.Comms.GetOrCreateGeneralChannel()
		if err != nil {
			return errResp(req.ID, "general channel: "+err.Error())
		}
		chID = gen.ID
	}
	msgs, err := d.Comms.ReadChannel(chID, time.Time{})
	if err != nil {
		return errResp(req.ID, err.Error())
	}
	result, _ := json.Marshal(msgs)
	return protocol.Response{ID: req.ID, Result: result}
}

func (d *Daemon) handleReadThread(req protocol.Request) protocol.Response {
	var p ReadThreadParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return errResp(req.ID, "bad params: "+err.Error())
	}
	msgs, err := d.Comms.ReadThread(p.ThreadID)
	if err != nil {
		return errResp(req.ID, err.Error())
	}
	result, _ := json.Marshal(msgs)
	return protocol.Response{ID: req.ID, Result: result}
}

func (d *Daemon) handleResolve(req protocol.Request) protocol.Response {
	var p ResolveParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return errResp(req.ID, "bad params: "+err.Error())
	}
	_, ws, err := d.Registry.ResolveByPath(p.Path)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return errResp(req.ID, "not registered")
		}
		return errResp(req.ID, "resolve: "+err.Error())
	}
	result, _ := json.Marshal(ResolveResult{Workspace: ws.Name, Project: filepath.Base(p.Path)})
	return protocol.Response{ID: req.ID, Result: result}
}

func (d *Daemon) handleWorkspaceAdd(req protocol.Request) protocol.Response {
	var p WorkspaceAddParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return errResp(req.ID, "bad params: "+err.Error())
	}
	ws, err := d.Registry.AddWorkspace(p.Name)
	if err != nil {
		return errResp(req.ID, err.Error())
	}
	result, _ := json.Marshal(map[string]any{"id": ws.ID})
	return protocol.Response{ID: req.ID, Result: result}
}

func (d *Daemon) handleProjectAdd(req protocol.Request) protocol.Response {
	var p ProjectAddParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return errResp(req.ID, "bad params: "+err.Error())
	}
	proj, err := d.Registry.AddProject(p.Workspace, p.Name, p.Path)
	if err != nil {
		return errResp(req.ID, err.Error())
	}
	result, _ := json.Marshal(ProjectAddResult{ID: proj.ID})
	return protocol.Response{ID: req.ID, Result: result}
}

func (d *Daemon) handleWorkspaceList(req protocol.Request) protocol.Response {
	names, err := d.Registry.ListWorkspaces()
	if err != nil {
		return errResp(req.ID, err.Error())
	}
	result, _ := json.Marshal(names)
	return protocol.Response{ID: req.ID, Result: result}
}

func (d *Daemon) handleProjectList(req protocol.Request) protocol.Response {
	var p ListParams
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return errResp(req.ID, "bad params: "+err.Error())
		}
	}
	var out []ProjectListItem
	if p.Workspace != "" {
		projs, err := d.Registry.ListProjects(p.Workspace)
		if err != nil {
			return errResp(req.ID, err.Error())
		}
		for _, pr := range projs {
			out = append(out, ProjectListItem{Workspace: p.Workspace, Name: pr.Name, Path: pr.Path})
		}
	} else {
		var err error
		out, err = d.Registry.ListAllProjects()
		if err != nil {
			return errResp(req.ID, err.Error())
		}
	}
	if out == nil {
		out = []ProjectListItem{}
	}
	result, _ := json.Marshal(out)
	return protocol.Response{ID: req.ID, Result: result}
}

func (d *Daemon) handleWorkspaceDelete(req protocol.Request) protocol.Response {
	var p WorkspaceAddParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return errResp(req.ID, "bad params: "+err.Error())
	}
	if err := d.Registry.DeleteWorkspace(p.Name); err != nil {
		if errors.Is(err, ErrNotFound) {
			return errResp(req.ID, "workspace "+p.Name+" not found")
		}
		return errResp(req.ID, err.Error())
	}
	return protocol.Response{ID: req.ID, Result: json.RawMessage(`{}`)}
}

func (d *Daemon) handleProjectDelete(req protocol.Request) protocol.Response {
	var p ProjectAddParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return errResp(req.ID, "bad params: "+err.Error())
	}
	if err := d.Registry.DeleteProject(p.Workspace, p.Name); err != nil {
		if errors.Is(err, ErrNotFound) {
			return errResp(req.ID, "project "+p.Workspace+"/"+p.Name+" not found")
		}
		return errResp(req.ID, err.Error())
	}
	return protocol.Response{ID: req.ID, Result: json.RawMessage(`{}`)}
}

func (d *Daemon) handleStatus(req protocol.Request) protocol.Response {
	var wsCount, projCount, sessCount int
	if err := d.Store.db.QueryRow("SELECT count(*) FROM workspaces").Scan(&wsCount); err != nil {
		return errResp(req.ID, "status: "+err.Error())
	}
	if err := d.Store.db.QueryRow("SELECT count(*) FROM projects").Scan(&projCount); err != nil {
		return errResp(req.ID, "status: "+err.Error())
	}
	if err := d.Store.db.QueryRow("SELECT count(*) FROM sessions WHERE status='active'").Scan(&sessCount); err != nil {
		return errResp(req.ID, "status: "+err.Error())
	}
	result, _ := json.Marshal(StatusResult{Running: true, Workspaces: wsCount, Projects: projCount, Sessions: sessCount, Sock: d.Sock})
	return protocol.Response{ID: req.ID, Result: result}
}

// ============================================
// Unix socket server
// ============================================

// Serve listens on the unix socket at d.Sock and dispatches requests.
// One json.Decoder per connection (addresses Task 1 minor: shared decoder
// would carry buffered state across requests).
func (d *Daemon) Serve(ctx context.Context) error {
	// Refuse to steal a live daemon's socket: if a connection succeeds, another
	// daemon is running. A stale socket file (no listener) is safe to remove.
	if conn, err := net.Dial("unix", d.Sock); err == nil {
		conn.Close()
		return fmt.Errorf("daemon already running on %s", d.Sock)
	}
	_ = os.Remove(d.Sock)
	ln, err := net.Listen("unix", d.Sock)
	if err != nil {
		return err
	}
	defer ln.Close()
	go func() {
		select {
		case <-ctx.Done():
		case <-d.shutdownCh:
		}
		ln.Close()
	}()
	if err := d.Store.RunRetention(); err != nil {
		log.Printf("retention: %v", err)
	}
	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-d.shutdownCh:
				return nil
			default:
			}
			return ctx.Err()
		}
		go d.serveConn(ctx, conn)
	}
}

func (d *Daemon) serveConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	dec := json.NewDecoder(conn)
	enc := json.NewEncoder(conn)
	for {
		var req protocol.Request
		if err := dec.Decode(&req); err != nil {
			return
		}
		resp := d.Handle(ctx, req)
		if err := enc.Encode(&resp); err != nil {
			log.Printf("write response: %v", err)
			return
		}
	}
}

// errResp is the standard error-response helper for all handlers.
func errResp(id int64, msg string) protocol.Response {
	return protocol.Response{ID: id, Error: &protocol.ResponseError{Message: msg}}
}
