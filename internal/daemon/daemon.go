package daemon

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/herdanis/his-mouse-friday/internal/config"
	"github.com/herdanis/his-mouse-friday/internal/protocol"
)

// generatePrefix returns 5 hex chars for a root session's name.
func generatePrefix() string {
	b := make([]byte, 3) // 3 bytes = 6 hex chars; truncate to 5.
	if _, err := rand.Read(b); err != nil {
		// Don't crash the wake path; prefix is just less collision-resistant.
		return "00000"
	}
	return hex.EncodeToString(b)[:5]
}

// resolveAgent picks the runtime for a wake: primary, falling back to
// secondary when primary's binary is missing or its model is unavailable.
// Failed/unknown probes mean "assume available" — never block spawn.
func (d *Daemon) resolveAgent(mouse *config.MouseConfig) (binary, model string) {
	type agent struct{ bin, model string }
	cands := []agent{{"opencode", "default"}}
	if mouse != nil && mouse.Agent.Primary.Provider != "" {
		cands[0] = agent{mouse.Agent.Primary.Provider, mouse.Agent.Primary.Model}
		if cands[0].model == "" {
			cands[0].model = "default"
		}
	}
	if mouse != nil && mouse.Agent.Secondary.Provider != "" {
		sec := agent{mouse.Agent.Secondary.Provider, mouse.Agent.Secondary.Model}
		if sec.model == "" {
			sec.model = "default"
		}
		if sec != cands[0] {
			cands = append(cands, sec)
		}
	}
	// Nil-safe: Daemon literals in tests may not set the injectables.
	look, probe := d.LookPath, d.ModelProbe
	if look == nil {
		look = exec.LookPath
	}
	if probe == nil {
		probe = runtimeModelAvailable
	}
	for _, c := range cands {
		if _, err := look(c.bin); err != nil {
			continue // binary not installed
		}
		if c.model != "default" {
			if ok, checkable := probe(c.bin, c.model); checkable && !ok {
				continue // model known missing
			}
		}
		return c.bin, c.model
	}
	logCapture(0, "resolveAgent: no candidate available, using primary %s/%s", cands[0].bin, cands[0].model)
	return cands[0].bin, cands[0].model
}

// ============================================
// Daemon
// ============================================

// Daemon owns all state and brokers agent-to-agent communication.
type Daemon struct {
	Store       *Store
	Registry    *Registry
	Sessions    *SessionStore
	Comms       *Comms
	Todos       *TodoStore
	Launcher    *Launcher
	MouseLoader func(string) (*config.MouseConfig, error)
	// SafetyNetEnabled: OnExit posts synthetic BLOCKED reply if agent exits
	// without one. Tests with /bin/echo set this false (echo exits too fast).
	SafetyNetEnabled bool
	// CaptureAgentSessionID injectable for tests; default is the real capture.
	CaptureAgentSessionID func(cfg SpawnConfig) (string, error)
	// LookPath + ModelProbe injectable for tests (resolveAgent fallback).
	LookPath   func(string) (string, error)
	ModelProbe func(binary, model string) (ok, checkable bool)
	Sock       string
	shutdownCh            chan struct{}
}

// NewDaemon wires a Daemon with default components for the given socket + db.
func NewDaemon(sock, dbPath string) (*Daemon, error) {
	store, err := OpenStore(dbPath)
	if err != nil {
		return nil, err
	}
	return &Daemon{
		Store:                 store,
		Registry:              &Registry{Store: store},
		Sessions:              &SessionStore{Store: store},
		Comms:                 &Comms{Store: store},
		Todos:                 &TodoStore{Store: store},
		Launcher:              &Launcher{},
		MouseLoader:           config.LoadMouse,
		SafetyNetEnabled:      true,
		CaptureAgentSessionID: captureAgentSessionID,
		LookPath:              exec.LookPath,
		ModelProbe:            runtimeModelAvailable,
		Sock:                  sock,
		shutdownCh:            make(chan struct{}),
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
	ParentID   int64  `json:"parent_id,omitempty"`
}
type PostResult struct {
	MessageID int64 `json:"message_id"`
}
type TaskStatusParams struct {
	MessageID int64 `json:"message_id"`
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
	MessageID int64 `json:"message_id"`
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

// SessionListItem is a row for the session_list RPC.
type SessionListItem struct {
	Name           string `json:"name"`
	Project        string `json:"project"`
	Status         string `json:"status"`
	AgentSessionID string `json:"session_id,omitempty"`
	ParentID   int64  `json:"parent_id,omitempty"`
	CreatedAt      string `json:"created_at"`
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
	case "session_list":
		return d.handleSessionList(req)
	case "todo_add", "todo_update", "todo_list", "todo_threads":
		return d.handleTodo(req)
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

// resolveToProject canonicalizes a `to` field. A full "workspace/project"
// passes through. A bare name resolves against the registry: 0 rows →
// not-found, 1 row → auto-resolved, 2+ rows → ambiguous error listing the
// candidates so the caller can ask the user to disambiguate.
func (d *Daemon) resolveToProject(to string) (string, error) {
	if strings.Contains(to, "/") {
		return to, nil
	}
	rows, err := d.Store.db.Query(
		`SELECT w.name, p.name FROM projects p JOIN workspaces w ON p.workspace_id=w.id
		 WHERE p.name=?`, to)
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", to, err)
	}
	defer rows.Close()
	var cands []string
	for rows.Next() {
		var ws, name string
		if err := rows.Scan(&ws, &name); err != nil {
			return "", fmt.Errorf("resolve %q: %w", to, err)
		}
		cands = append(cands, ws+"/"+name)
	}
	switch len(cands) {
	case 0:
		return "", fmt.Errorf("no project named %q; call list_project_agents", to)
	case 1:
		return cands[0], nil
	default:
		return "", fmt.Errorf("ambiguous %q: exists in %s — specify workspace/project", to, strings.Join(cands, ", "))
	}
}

// threadSessionActive reports whether the thread's latest session is still
// running. Queries root_thread_id (not task_msg_id) to catch reply-wakes too.
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
	// Auto-fill `to` on thread replies that omitted it: address whoever the
	// thread root didn't come from, so a bare reply on an existing thread
	// still wakes the right project instead of silently going nowhere.
	// Skip on "done" — that's the worker's own completion notice, and
	// auto-resolving it back to the (often registered) originator would
	// wake that project's agent too, an unrequested reply-loop.
	if p.To == "" && p.ThreadID != 0 && p.Status != "done" {
		if root, err := d.Comms.GetMessage(p.ThreadID); err == nil {
			other := root.ToProject
			if other == "" || other == p.From {
				other = root.FromProject
			}
			if other != "" && other != p.From {
				p.To = other
			}
		}
	}
	// Canonicalize `to` before save: bare names resolve against the registry
	// (1 → auto, 2+ → ambiguous error listing candidates). Stored msg stays canonical.
	if p.To != "" {
		resolved, err := d.resolveToProject(p.To)
		if err != nil {
			return errResp(req.ID, err.Error())
		}
		p.To = resolved
	}
	msg, err := d.Comms.PostMessage(chID, p.ThreadID, p.From, p.To, p.Content, p.Status)
	if err != nil {
		return errResp(req.ID, err.Error())
	}
	// Wake to-addressed messages unless the thread's agent is still running.
	// A prior done reply IS re-wakeable — follow-up resumes the prior session.
	if p.To != "" {
		if d.threadSessionActive(p.ThreadID) {
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

// wakeAgent spawns the addressed project agent so it can read the thread and
// reply. Serverless: boot per mention, handle, reply in-thread, exit.
func (d *Daemon) wakeAgent(ctx context.Context, p PostParams, msg Message) error {
	parts := strings.SplitN(msg.ToProject, "/", 2)
	if len(parts) != 2 {
		return fmt.Errorf("to must be workspace/project, got %q", msg.ToProject)
	}
	proj, err := d.findProject(parts[0], parts[1])
	if err != nil {
		// Unregistered recipient — post but don't wake (mailbox semantics).
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
	binary, model := d.resolveAgent(mouse)
	logCapture(0, "wake %s: agent=%s model=%s", msg.ToProject, binary, model)
	runbook, _ := os.ReadFile(filepath.Join(proj.Path, "MOUSE.md"))
	// parentID: explicit (cross-project delegation), else msg.ID (root) or
	// msg.ThreadID (reply wake).
	parentID := msg.ID
	if p.ParentID != 0 {
		parentID = p.ParentID
	} else if msg.ThreadID != 0 {
		parentID = msg.ThreadID
	}
	// Prefix: lookup an existing one for this root (inherit), else generate.
	var prefix string
	var existingName sql.NullString
	err = d.Store.db.QueryRow(
		`SELECT prefix, name FROM sessions WHERE root_thread_id=? AND prefix IS NOT NULL ORDER BY id ASC LIMIT 1`,
		parentID).Scan(&prefix, &existingName)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("prefix lookup: %w", err)
	}
	if prefix == "" {
		prefix = generatePrefix()
	}
	name := prefix + "-" + proj.Name
	tmpSess, err := d.Sessions.Create(proj.ID, binary, model, 0, msg.ID, parentID, prefix, name)
	if err != nil {
		return fmt.Errorf("session: %w", err)
	}
	// Resume prior session on this thread — scoped to the resolved binary, so
	// a runtime fallback (e.g. opencode -> claude) never resumes a foreign
	// runtime's session id.
	var priorOcID sql.NullString
	err = d.Store.db.QueryRow(
		`SELECT opencode_session_id FROM sessions WHERE root_thread_id=? AND agent_binary=? AND opencode_session_id IS NOT NULL AND opencode_session_id != '' ORDER BY id DESC LIMIT 1`,
		parentID, binary).Scan(&priorOcID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("resume lookup: %w", err)
	}
	entrypoint := fmt.Sprintf(
		"[DELEGATED TASK] Parent agent %s sent you a message (id %d) in the general channel.\n"+
			"1. Call the read_thread MCP tool with message_id=%d to load the task and prior context.\n"+
			"2. Do the work inside this project directory.\n"+
			"3. Track work items: todo_list with thread_id=%d (see existing), todo_add for new steps, todo_update to mark done.\n"+
			"4. Reply: post_message with thread_id=%d, status=\"done\", and a one-line summary.",
		msg.FromProject, msg.ID, parentID, parentID, parentID)
	spawnCfg := SpawnConfig{
		Dir:            proj.Path,
		Binary:         binary,
		Model:          model,
		Runbook:        string(runbook),
		Task:           entrypoint,
		FromID:         msg.FromProject,
		ProjectID:      msg.ToProject,
		ChannelID:      msg.ChannelID,
		SessionID:      tmpSess.ID,
		TaskMsgID:      msg.ID,
		SessionName:    name,
		AgentSessionID: priorOcID.String,
		OnExit: func(code int) {
			d.Sessions.MarkExited(tmpSess.ID, code)
			if !d.SafetyNetEnabled {
				return
			}
			// Safety net: post BLOCKED reply if agent exited without one.
			// Covers crashes, kills, and silent exits from bash:ask walls.
			var doneCount int
			d.Store.db.QueryRow(
				`SELECT count(*) FROM messages WHERE thread_id=? AND status='done'`, parentID).Scan(&doneCount)
			if doneCount == 0 {
				content := fmt.Sprintf("BLOCKED: agent exited (code=%d) without posting a done reply", code)
				if _, err := d.Comms.PostMessage(msg.ChannelID, parentID, msg.ToProject, msg.FromProject, content, "done"); err != nil {
					log.Printf("synthetic done reply for thread %d: %v", parentID, err)
				}
			}
		},
	}
	pid, err := d.Launcher.Spawn(ctx, spawnCfg)
	if err != nil {
		d.Sessions.SetStatus(tmpSess.ID, "failed")
		return fmt.Errorf("spawn: %w", err)
	}
	d.Sessions.SetPID(tmpSess.ID, pid)
	d.Sessions.SetStatus(tmpSess.ID, "active")
	if spawnCfg.AgentSessionID != "" {
		// Resume: ocID already known, set directly — no capture needed.
		if err := d.Sessions.SetAgentSessionID(tmpSess.ID, spawnCfg.AgentSessionID); err != nil {
			log.Printf("resume SetAgentSessionID session %d: %v", tmpSess.ID, err)
		}
		return nil
	}
	// Fresh spawn: opencode registers the session async after cmd.Start(), so
	// poll for it by title up to 10s (fixed 2s was too short → empty SESSION).
	go func() {
		const (
			pollEvery = 500 * time.Millisecond
			maxWait   = 10 * time.Second
		)
		deadline := time.Now().Add(maxWait)
		var ocID string
		var err error
		for time.Now().Before(deadline) {
			time.Sleep(pollEvery)
			ocID, err = d.CaptureAgentSessionID(spawnCfg)
			if err == nil && ocID != "" {
				break
			}
		}
		// Debug log to file — daemon stderr may be discarded by ensureDaemon.
		logCapture(tmpSess.ID, "name=%q binary=%q dir=%q ocID=%q err=%v",
			spawnCfg.SessionName, spawnCfg.Binary, spawnCfg.Dir, ocID, err)
		if err != nil || ocID == "" {
			return
		}
		if err := d.Sessions.SetAgentSessionID(tmpSess.ID, ocID); err != nil {
			logCapture(tmpSess.ID, "SetAgentSessionID error: %v", err)
		}
	}()
	return nil
}

// logCapture appends a debug line to <state dir>/capture.log. Best-effort.
func logCapture(sessionID int64, format string, args ...any) {
	f, err := os.OpenFile(filepath.Join(protocol.StateDir(), "capture.log"),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0640)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "session %d: "+format+"\n", append([]any{sessionID}, args...)...)
}

// handleTaskStatus reports agent state (working/exited/failed/no_agent) + done
// reply presence. message_id resolves to thread root; done check scoped to
// replies at or after the passed message so prior tasks don't false-complete.
func (d *Daemon) handleTaskStatus(req protocol.Request) protocol.Response {
	var p TaskStatusParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return errResp(req.ID, "bad params: "+err.Error())
	}
	if p.MessageID == 0 {
		return errResp(req.ID, "message_id is required")
	}
	// Resolve message to thread root: reply → thread_id, root → id.
	var parentID int64
	err := d.Store.db.QueryRow(
		`SELECT IFNULL(thread_id, id) FROM messages WHERE id=?`, p.MessageID).Scan(&parentID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			result, _ := json.Marshal(TaskStatusResult{HasDone: false, AgentStatus: "no_agent"})
			return protocol.Response{ID: req.ID, Result: result}
		}
		return errResp(req.ID, err.Error())
	}
	// Done check: done replies on this thread at or after the passed message.
	var doneCount int
	d.Store.db.QueryRow(
		`SELECT count(*) FROM messages WHERE thread_id=? AND status='done' AND id >= ?`,
		parentID, p.MessageID).Scan(&doneCount)

	var sessID int64
	var status string
	var pid sql.NullInt64
	var exitCode sql.NullInt64
	err = d.Store.db.QueryRow(
		`SELECT id, status, pid, exit_code FROM sessions WHERE root_thread_id=? ORDER BY id DESC LIMIT 1`,
		parentID).Scan(&sessID, &status, &pid, &exitCode)
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
	if p.MessageID == 0 {
		return errResp(req.ID, "message_id is required")
	}
	// Resolve to thread root: if reply (thread_id set), use that; if root, use id.
	var parentID int64
	err := d.Store.db.QueryRow(
		`SELECT IFNULL(thread_id, id) FROM messages WHERE id=?`, p.MessageID).Scan(&parentID)
	if err != nil {
		return errResp(req.ID, "message not found")
	}
	msgs, err := d.Comms.ReadThread(parentID)
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

// handleSessionList returns all sessions joined to project, newest first.
// IFNULL coerces NULL ocID/root to ""/0 so the CLI renders clean dashes.
func (d *Daemon) handleSessionList(req protocol.Request) protocol.Response {
	rows, err := d.Store.db.Query(
		`SELECT IFNULL(s.name,'-'), p.name, s.status, IFNULL(s.opencode_session_id,'') AS session_id, IFNULL(s.root_thread_id,0) AS parent_id, IFNULL(s.created_at,'')
		 FROM sessions s JOIN projects p ON s.project_id=p.id
		 ORDER BY s.id DESC`)
	if err != nil {
		return errResp(req.ID, err.Error())
	}
	defer rows.Close()
	var out []SessionListItem
	for rows.Next() {
		var it SessionListItem
		if err := rows.Scan(&it.Name, &it.Project, &it.Status, &it.AgentSessionID, &it.ParentID, &it.CreatedAt); err != nil {
			return errResp(req.ID, err.Error())
		}
		out = append(out, it)
	}
	if out == nil {
		out = []SessionListItem{}
	}
	result, _ := json.Marshal(out)
	return protocol.Response{ID: req.ID, Result: result}
}

// ============================================
// Todos — RPC handlers
// ============================================

func (d *Daemon) handleTodo(req protocol.Request) protocol.Response {
	var p struct {
		ThreadID int64  `json:"thread_id"`
		Content  string `json:"content"`
		ID       int64  `json:"id"`
		State    string `json:"state"`
	}
	// todo_threads takes no params; guard so an empty/nil body doesn't error.
	if req.Method != "todo_threads" || len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return errResp(req.ID, "bad params: "+err.Error())
		}
	}
	switch req.Method {
	case "todo_add":
		if p.ThreadID == 0 || p.Content == "" {
			return errResp(req.ID, "todo_add needs thread_id and content")
		}
		td, err := d.Todos.Add(p.ThreadID, p.Content)
		if err != nil {
			return errResp(req.ID, fmt.Sprintf("no thread %d: %s", p.ThreadID, err))
		}
		b, _ := json.Marshal(map[string]int64{"id": td.ID})
		return protocol.Response{ID: req.ID, Result: b}
	case "todo_update":
		if err := d.Todos.Update(p.ID, p.State); err != nil {
			return errResp(req.ID, err.Error())
		}
		b, _ := json.Marshal(map[string]bool{"ok": true})
		return protocol.Response{ID: req.ID, Result: b}
	case "todo_threads":
		return d.handleTodoThreads(req)
	default: // todo_list
		todos, err := d.Todos.List(p.ThreadID)
		if err != nil {
			return errResp(req.ID, err.Error())
		}
		if todos == nil {
			todos = []Todo{}
		}
		b, _ := json.Marshal(todos)
		return protocol.Response{ID: req.ID, Result: b}
	}
}

// handleTodoThreads lists threads that have todos, with preview + counts.
func (d *Daemon) handleTodoThreads(req protocol.Request) protocol.Response {
	rows, err := d.Store.db.Query(`
		SELECT t.thread_id, m.content,
		       SUM(CASE WHEN t.state='done' THEN 1 ELSE 0 END), COUNT(*)
		FROM todos t JOIN messages m ON m.id = t.thread_id
		GROUP BY t.thread_id ORDER BY t.thread_id DESC`)
	if err != nil {
		return errResp(req.ID, err.Error())
	}
	defer rows.Close()
	type row struct {
		ThreadID int64  `json:"thread_id"`
		Preview  string `json:"preview"`
		Done     int    `json:"done"`
		Total    int    `json:"total"`
	}
	var out []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.ThreadID, &r.Preview, &r.Done, &r.Total); err != nil {
			return errResp(req.ID, err.Error())
		}
		if runes := []rune(r.Preview); len(runes) > 60 {
			r.Preview = string(runes[:57]) + "..."
		}
		out = append(out, r)
	}
	if out == nil {
		out = []row{}
	}
	b, _ := json.Marshal(out)
	return protocol.Response{ID: req.ID, Result: b}
}

// ============================================
// Unix socket server
// ============================================

// Serve listens on d.Sock and dispatches requests. One json.Decoder per
// connection (shared decoder carries buffered state across requests).
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
