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
	"regexp"
	"strings"
	"syscall"
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
func (d *Daemon) resolveAgent(mouse *config.MouseConfig) (binary, model, agentName string) {
	type agent struct{ bin, model, name string }
	cands := []agent{{"opencode", "default", ""}}
	if mouse != nil && mouse.Agent.Primary.Provider != "" {
		cands[0] = agent{mouse.Agent.Primary.Provider, mouse.Agent.Primary.Model, mouse.Agent.Primary.Name}
		if cands[0].model == "" {
			cands[0].model = "default"
		}
	}
	if mouse != nil && mouse.Agent.Secondary.Provider != "" {
		sec := agent{mouse.Agent.Secondary.Provider, mouse.Agent.Secondary.Model, mouse.Agent.Secondary.Name}
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
		return c.bin, c.model, c.name
	}
	logf("wake", "no runtime candidate available, falling back to primary %s/%s", cands[0].bin, cands[0].model)
	return cands[0].bin, cands[0].model, cands[0].name
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
	shutdownCh chan struct{}
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
	ParentID int64  `json:"parent_id,omitempty"`
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
	// NextAction spells out what to do with this result. A terminal result
	// returns instantly (nothing left to wait for), so a caller that doesn't
	// recognise it as final re-calls at full speed — an in-band instruction
	// stops that better than a passive boolean.
	NextAction string `json:"next_action"`

	// Progress detail. The child runs in its own process and can't write into
	// the caller's session, so this is how its work becomes visible there.
	Project     string     `json:"project,omitempty"`      // workspace/project doing the work
	ElapsedSecs int64      `json:"elapsed_secs,omitempty"` // since the task was posted
	TodosDone   int        `json:"todos_done"`
	TodosTotal  int        `json:"todos_total"`
	CurrentStep string     `json:"current_step,omitempty"` // first not-done todo
	Todos       []TodoLine `json:"todos,omitempty"`
	LastUpdate  string     `json:"last_update,omitempty"` // most recent reply on the thread

	// Child's own account of where it is. ProgressNote and ETAMinutes are its
	// claims; ProgressAgeSecs is fact, and the two together are what separate
	// "on track" from "said 10min, went quiet 40min ago".
	ProgressNote    string `json:"progress_note,omitempty"`
	ETAMinutes      int    `json:"eta_minutes,omitempty"`
	ProgressAgeSecs int64  `json:"progress_age_secs,omitempty"`
}

// TodoLine is one work item as the caller sees it.
type TodoLine struct {
	Content string `json:"content"`
	State   string `json:"state"`
}

// nextAction renders the caller's next step for a status snapshot.
func nextAction(hasDone bool, agentStatus string, messageID int64) string {
	switch {
	case hasDone:
		return fmt.Sprintf("COMPLETE — stop polling. Call read_thread(message_id=%d) to read the result.", messageID)
	case agentStatus == "exited" || agentStatus == "failed":
		return fmt.Sprintf("ENDED WITHOUT A DONE REPLY — stop polling. Call read_thread(message_id=%d); "+
			"if there is no reply, re-dispatch with post_message.", messageID)
	case agentStatus == "no_agent":
		return "NO AGENT WAS WOKEN — stop polling. Re-post with a `to` field to spawn one."
	default:
		return "STILL WORKING — see todos/current_step for what it's doing. Call task_status again for this message_id; it blocks up to 5min for you, so do not sleep."
	}
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
	ParentID       int64  `json:"parent_id,omitempty"`
	CreatedAt      string `json:"created_at"`
	FinishedAt     string `json:"finished_at,omitempty"` // empty while still running
	// EngagedBy is who asked for this session's work — the sender of the
	// message that spawned it. On a chain (A engages B, B engages C) every
	// session shares one root thread, so this edge is what recovers the shape.
	EngagedBy string `json:"engaged_by,omitempty"`
	PID       int    `json:"pid,omitempty"`
	// Dir is the project path. opencode resumes per-directory, so the session
	// id alone is not enough to reopen a session.
	Dir string `json:"dir,omitempty"`
}

// ============================================
// Method dispatch
// ============================================

// Read-only polling (monitor TUI, task_status) logs one summary line instead
// of full bodies — otherwise it drowns the wake/spawn events in the file.
var quietMethods = map[string]bool{
	"read_thread": true, "read_channel": true, "todo_list": true,
	"todo_threads": true, "session_list": true, "status": true,
	"task_status": true, "project_list": true, "workspace_list": true,
	"list_project_agents": true,
}

// Handle dispatches a single request, logging both ends of it.
func (d *Daemon) Handle(ctx context.Context, req protocol.Request) protocol.Response {
	start := time.Now()
	quiet := quietMethods[req.Method]
	if !quiet {
		logf("rpc", "recv id=%d method=%s params=%s", req.ID, req.Method, trunc(string(req.Params), 2000))
	}
	resp := d.dispatch(ctx, req)
	dur := time.Since(start).Round(time.Millisecond)
	switch {
	case resp.Error != nil:
		logErrf("rpc", "id=%d method=%s dur=%s params=%s: %s", req.ID, req.Method, dur, trunc(string(req.Params), 500), resp.Error.Message)
	case quiet:
		logf("rpc", "id=%d method=%s dur=%s ok (%d bytes)", req.ID, req.Method, dur, len(resp.Result))
	default:
		logf("rpc", "send id=%d method=%s dur=%s result=%s", req.ID, req.Method, dur, trunc(string(resp.Result), 2000))
	}
	return resp
}

func (d *Daemon) dispatch(ctx context.Context, req protocol.Request) protocol.Response {
	switch req.Method {
	case "post_message":
		return d.handlePost(ctx, req)
	case "task_status":
		return d.handleTaskStatus(ctx, req)
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
	case "thread_delete":
		return d.handleThreadDelete(req)
	case "prune":
		return d.handlePrune(req)
	case "report_progress":
		return d.handleReportProgress(req)
	case "todo_add", "todo_update", "todo_list", "todo_threads", "todo_delete":
		return d.handleTodo(req)
	case "shutdown":
		select {
		case <-d.shutdownCh:
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

// threadSessionActive reports whether toProject already has a session running
// on this thread. Scoped per project, not per thread: several projects can
// work on one parent task at once, and a thread-wide check would silently
// swallow the second project's wake.
func (d *Daemon) threadSessionActive(threadID int64, toProject string) bool {
	if threadID == 0 {
		return false
	}
	parts := strings.SplitN(toProject, "/", 2)
	if len(parts) != 2 {
		return false
	}
	var status string
	err := d.Store.db.QueryRow(
		`SELECT s.status FROM sessions s
		 JOIN projects p ON s.project_id=p.id
		 JOIN workspaces w ON p.workspace_id=w.id
		 WHERE s.root_thread_id=? AND w.name=? AND p.name=?
		 ORDER BY s.id DESC LIMIT 1`,
		threadID, parts[0], parts[1]).Scan(&status)
	if err != nil {
		return false // this project has no session on the thread
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
			// Only ever auto-fill a real "workspace/project". A thread opened
			// from an unregistered directory carries a non-project identity
			// there, and resolving that as a recipient would fail the post.
			if other != "" && other != p.From && strings.Contains(other, "/") {
				p.To = other
				logf("post", "thread=%d autofilled to=%s from thread root", p.ThreadID, p.To)
			}
		}
	}
	// Canonicalize `to` before save: bare names resolve against the registry
	// (1 → auto, 2+ → ambiguous error listing candidates). Stored msg stays canonical.
	if p.To != "" {
		resolved, err := d.resolveToProject(p.To)
		if err != nil {
			logErrf("post", "resolve to=%q: %v", p.To, err)
			return errResp(req.ID, err.Error())
		}
		if resolved != p.To {
			logf("post", "resolved to=%q -> %s", p.To, resolved)
		}
		p.To = resolved
	}
	msg, err := d.Comms.PostMessage(chID, p.ThreadID, p.From, p.To, p.Content, p.Status)
	if err != nil {
		logErrf("post", "thread=%d channel=%d from=%s to=%s save failed: %v", p.ThreadID, chID, p.From, p.To, err)
		return errResp(req.ID, err.Error())
	}
	logf("post", "thread=%d msg=%d channel=%d from=%s to=%s status=%q content=%q",
		threadKey(p.ThreadID, msg.ID), msg.ID, chID, p.From, p.To, p.Status, trunc(p.Content, 500))
	// Wake to-addressed messages unless the thread's agent is still running.
	// A prior done reply IS re-wakeable — follow-up resumes the prior session.
	//
	// A "done" never wakes: it is the worker announcing it finished, and
	// spawning the originator to receive that is a whole agent process doing
	// nothing. The originator learns via task_status, not a wake. Guarding
	// only the `to` auto-fill above misses this — `hmf done` sets `to`
	// explicitly from HMF_FROM, so every completed task spawned a pointless
	// parent-side agent.
	if p.To == "" || p.Status == "done" {
		logf("wake", "thread=%d msg=%d no wake (to=%q status=%q)", threadKey(p.ThreadID, msg.ID), msg.ID, p.To, p.Status)
	} else if d.threadSessionActive(p.ThreadID, p.To) {
		logf("wake", "thread=%d msg=%d suppressed — %s already has an active session", p.ThreadID, msg.ID, p.To)
	} else {
		if err := d.wakeAgent(ctx, p, msg); err != nil {
			logErrf("wake", "thread=%d msg=%d to=%s wake failed: %v", threadKey(p.ThreadID, msg.ID), msg.ID, p.To, err)
			return errResp(req.ID, "wake: "+err.Error())
		}
	}
	result, _ := json.Marshal(PostResult{MessageID: msg.ID})
	return protocol.Response{ID: req.ID, Result: result}
}

// checkOutboundAllowed rejects a wake when the sending project's mouse.yaml
// sets a2a.allow_outbound: false. Mirrors the inbound guard — both sides of an
// A2A hop declare consent, and both are enforced here.
//
// Anything that isn't a registered "workspace/project" with a mouse.yaml is
// allowed through: humans and scratch dirs aren't governed by a repo's config.
func (d *Daemon) checkOutboundAllowed(fromProject string) error {
	parts := strings.SplitN(fromProject, "/", 2)
	if len(parts) != 2 {
		return nil
	}
	proj, err := d.findProject(parts[0], parts[1])
	if err != nil {
		return nil // unregistered sender — nothing declared, nothing to enforce
	}
	mouse, err := d.MouseLoader(filepath.Join(proj.Path, "mouse.yaml"))
	if err != nil || mouse == nil {
		return nil // no config = open mode, same as inbound
	}
	if !mouse.A2A.AllowOutbound {
		return fmt.Errorf("project %s does not allow outbound engagement (a2a.allow_outbound is false in its mouse.yaml)", fromProject)
	}
	return nil
}

// wakeAgent spawns the addressed project agent so it can read the thread and
// reply. Serverless: boot per mention, handle, reply in-thread, exit.
func (d *Daemon) wakeAgent(ctx context.Context, p PostParams, msg Message) error {
	// parentID: explicit (cross-project delegation), else msg.ID (root) or
	// msg.ThreadID (reply wake). Computed first so every log line carries it.
	parentID := msg.ID
	if p.ParentID != 0 {
		parentID = p.ParentID
	} else if msg.ThreadID != 0 {
		parentID = msg.ThreadID
	}
	// Every line of one wake shares these keys: `grep 'thread=68' hmf.log`
	// replays the whole task.
	wlog := func(format string, args ...any) {
		logf("wake", "thread=%d msg=%d to=%s "+format, append([]any{parentID, msg.ID, msg.ToProject}, args...)...)
	}
	wlog("start from=%s", msg.FromProject)
	parts := strings.SplitN(msg.ToProject, "/", 2)
	if len(parts) != 2 {
		return fmt.Errorf("to must be workspace/project, got %q", msg.ToProject)
	}
	proj, err := d.findProject(parts[0], parts[1])
	if err != nil {
		// Unregistered recipient — post but don't wake (mailbox semantics).
		if errors.Is(err, sql.ErrNoRows) {
			wlog("not registered — mailbox only, no spawn")
			return nil
		}
		return fmt.Errorf("project %s not found: %w", msg.ToProject, err)
	}
	// Outbound consent: a registered sender may forbid delegating out. Checked
	// before inbound so the refusal names the side that actually said no.
	// Unregistered senders (a human orchestrating from a scratch dir) have no
	// mouse.yaml and are unrestricted.
	if err := d.checkOutboundAllowed(msg.FromProject); err != nil {
		return err
	}
	mouse, err := d.MouseLoader(filepath.Join(proj.Path, "mouse.yaml"))
	if err != nil {
		return fmt.Errorf("read mouse.yaml: %w", err)
	}
	if mouse != nil && !mouse.A2A.AllowInbound {
		return fmt.Errorf("project %s does not allow inbound engagement", msg.ToProject)
	}
	binary, model, agentName := d.resolveAgent(mouse)
	wlog("dir=%s agent=%s model=%s agent_name=%s", proj.Path, binary, model, agentName)
	runbook, _ := os.ReadFile(filepath.Join(proj.Path, "MOUSE.md"))
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
	wlog("session=%d created name=%s", tmpSess.ID, name)
	// Resume scoped to binary + project: another project's session id doesn't exist in this dir.
	var priorOcID sql.NullString
	err = d.Store.db.QueryRow(
		`SELECT opencode_session_id FROM sessions WHERE root_thread_id=? AND agent_binary=? AND project_id=? AND opencode_session_id IS NOT NULL AND opencode_session_id != '' ORDER BY id DESC LIMIT 1`,
		parentID, binary, proj.ID).Scan(&priorOcID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("resume lookup: %w", err)
	}
	entrypoint := fmt.Sprintf(
		"[DELEGATED TASK] Parent agent %s sent you a message (id %d) in the general channel.\n"+
			"1. Call the read_thread MCP tool with message_id=%d to load the task and prior context.\n"+
			"2. Do the work inside this project directory. The task states intent; you own the\n"+
			"   details. Locate the code, decide the files, and carry it through — including\n"+
			"   any file count the task implies. Read this project's MOUSE.md/AGENTS.md for\n"+
			"   local conventions and its verify commands, and run them before replying.\n"+
			"3. Track work items: todo_list with thread_id=%d (see existing), todo_add for new steps, todo_update to mark done.\n"+
			"4. Say where you are. The parent cannot see this session — silence is\n"+
			"   indistinguishable from being stuck. Once you know the job runs longer than a\n"+
			"   couple of minutes, call report_progress with what you are doing and\n"+
			"   eta_minutes; call it again whenever that estimate moves or before a long\n"+
			"   quiet stretch. It wakes nobody and costs the parent nothing.\n"+
			"5. Reply: post_message with thread_id=%d, status=\"done\", a one-line summary, the\n"+
			"   files you changed, and the verify result. The parent trusts this reply instead\n"+
			"   of re-reading your files, so it must be accurate. Blocked → start with \"BLOCKED: \".",
		msg.FromProject, msg.ID, parentID, parentID, parentID)
	spawnCfg := SpawnConfig{
		Dir:       proj.Path,
		Binary:    binary,
		Model:     model,
		AgentName: agentName,
		Runbook:   string(runbook),
		Task:      entrypoint,
		FromID:    msg.FromProject,
		ProjectID: msg.ToProject,
		ChannelID: msg.ChannelID,
		SessionID: tmpSess.ID,
		// parentID, not msg.ID — must match the root task_status resolves to.
		TaskMsgID:      parentID,
		SessionName:    name,
		AgentSessionID: priorOcID.String,
		OnExit: func(code int) {
			logf("exit", "thread=%d session=%d to=%s name=%s process exited code=%d", parentID, tmpSess.ID, msg.ToProject, name, code)
			var doneCount int
			d.Store.db.QueryRow(
				`SELECT count(*) FROM messages WHERE thread_id=? AND status='done'`, parentID).Scan(&doneCount)
			// Done reply already landed — don't let a nonzero exit code
			// (e.g. killing a resumed session that won't self-exit) mark it failed.
			effectiveCode := code
			if doneCount > 0 {
				effectiveCode = 0
			}
			d.Sessions.MarkExited(tmpSess.ID, effectiveCode)
			if !d.SafetyNetEnabled {
				return
			}
			// Safety net: post BLOCKED reply if agent exited without one.
			// Covers crashes, kills, and silent exits from bash:ask walls.
			// Conditional insert, not the doneCount read above: the agent may
			// post its real reply between that read and this write.
			if doneCount == 0 {
				logf("exit", "thread=%d session=%d no done reply — posting safety-net BLOCKED", parentID, tmpSess.ID)
				content := fmt.Sprintf("BLOCKED: agent exited (code=%d) without posting a done reply", code)
				if _, err := d.Comms.PostBlockedIfNoDone(msg.ChannelID, parentID, msg.ToProject, msg.FromProject, content); err != nil {
					logErrf("exit", "thread=%d session=%d safety-net post failed: %v", parentID, tmpSess.ID, err)
				}
			}
		},
	}
	pid, err := d.Launcher.Spawn(ctx, spawnCfg)
	if err != nil {
		d.Sessions.SetStatus(tmpSess.ID, "failed")
		logErrf("spawn", "thread=%d session=%d name=%s spawn failed: %v", parentID, tmpSess.ID, name, err)
		return fmt.Errorf("spawn: %w", err)
	}
	d.Sessions.SetPID(tmpSess.ID, pid)
	d.Sessions.SetStatus(tmpSess.ID, "active")
	logf("spawn", "thread=%d session=%d name=%s pid=%d resume=%q", parentID, tmpSess.ID, name, pid, spawnCfg.AgentSessionID)
	// Acknowledge the pickup from the daemon, not the agent. An LLM asked to
	// "reply that you started" can't report the failure where it never ran, so
	// the one signal that distinguishes "never spawned" from "working quietly"
	// has to come from the side that knows the process exists.
	ack := fmt.Sprintf("working on it — agent spawned as %s (pid %d)", spawnCfg.SessionName, pid)
	if _, err := d.Comms.PostMessage(msg.ChannelID, parentID, msg.ToProject, msg.FromProject, ack, "ack"); err != nil {
		logErrf("wake", "thread=%d ack post failed: %v", parentID, err)
	}
	if spawnCfg.AgentSessionID != "" {
		// Resume: ocID already known, set directly — no capture needed.
		if err := d.Sessions.SetAgentSessionID(tmpSess.ID, spawnCfg.AgentSessionID); err != nil {
			logErrf("wake", "thread=%d session=%d resume SetAgentSessionID: %v", parentID, tmpSess.ID, err)
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
		logf("capture", "thread=%d session=%d name=%s agent_session=%q err=%v",
			parentID, tmpSess.ID, spawnCfg.SessionName, ocID, err)
		if err != nil || ocID == "" {
			return
		}
		if err := d.Sessions.SetAgentSessionID(tmpSess.ID, ocID); err != nil {
			logErrf("capture", "thread=%d session=%d SetAgentSessionID: %v", parentID, tmpSess.ID, err)
		}
	}()
	return nil
}

// logCapture logs a session-scoped debug line into the daemon event log.
func logCapture(sessionID int64, format string, args ...any) {
	logf("capture", "session %d: "+format, append([]any{sessionID}, args...)...)
}

// taskStatusWait is how long a task_status call blocks before reporting back.
// Fixed, not caller-tunable: a shorter wait just produces more round trips for
// the same answer. var (not const) so tests can shorten it.
var taskStatusWait = 5 * time.Minute

// taskStatusPollInterval is how often a blocking task_status re-checks the DB.
const taskStatusPollInterval = 2 * time.Second

// handleTaskStatus reports agent state (working/exited/failed/no_agent) +
// done reply presence. Always blocks for taskStatusWait, returning early the
// moment the task reaches a terminal state. Blocking IS the pacing mechanism —
// a call that returns instantly just invites an immediate retry.
func (d *Daemon) handleTaskStatus(ctx context.Context, req protocol.Request) protocol.Response {
	var p TaskStatusParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return errResp(req.ID, "bad params: "+err.Error())
	}
	if p.MessageID == 0 {
		return errResp(req.ID, "message_id is required")
	}
	deadline := time.Now().Add(taskStatusWait)
	for {
		result, terminal, err := d.computeTaskStatus(p.MessageID)
		if err != nil {
			return errResp(req.ID, err.Error())
		}
		if terminal || !time.Now().Before(deadline) {
			b, _ := json.Marshal(result)
			return protocol.Response{ID: req.ID, Result: b}
		}
		select {
		case <-ctx.Done():
			b, _ := json.Marshal(result)
			return protocol.Response{ID: req.ID, Result: b}
		case <-time.After(taskStatusPollInterval):
		}
	}
}

// computeTaskStatus is the instant (non-blocking) status snapshot.
// terminal=true means further polling won't change the answer.
func (d *Daemon) computeTaskStatus(messageID int64) (result TaskStatusResult, terminal bool, err error) {
	// Resolve message to thread root: reply → thread_id, root → id.
	var parentID int64
	err = d.Store.db.QueryRow(
		`SELECT IFNULL(thread_id, id) FROM messages WHERE id=?`, messageID).Scan(&parentID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return TaskStatusResult{HasDone: false, AgentStatus: "no_agent",
				NextAction: nextAction(false, "no_agent", messageID)}, true, nil
		}
		return TaskStatusResult{}, true, err
	}
	// Done check: done replies on this thread at or after the passed message.
	var doneCount int
	d.Store.db.QueryRow(
		`SELECT count(*) FROM messages WHERE thread_id=? AND status='done' AND id >= ?`,
		parentID, messageID).Scan(&doneCount)

	var sessID int64
	var status string
	var pid sql.NullInt64
	var exitCode sql.NullInt64
	err = d.Store.db.QueryRow(
		`SELECT id, status, pid, exit_code FROM sessions WHERE root_thread_id=? ORDER BY id DESC LIMIT 1`,
		parentID).Scan(&sessID, &status, &pid, &exitCode)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Terminal: the wake already ran (synchronously, before the caller
			// got this message_id), so no session means none is coming.
			// Blocking here would just wait out the full timeout for nothing.
			hasDone := doneCount > 0
			return TaskStatusResult{HasDone: hasDone, AgentStatus: "no_agent",
				NextAction: nextAction(hasDone, "no_agent", messageID)}, true, nil
		}
		return TaskStatusResult{}, true, err
	}
	// "active" → "working"; other statuses pass through.
	agentStatus := status
	if status == "active" {
		agentStatus = "working"
	}
	hasDone := doneCount > 0
	terminal = hasDone || agentStatus == "exited" || agentStatus == "failed"
	res := TaskStatusResult{
		HasDone:     hasDone,
		AgentStatus: agentStatus,
		SessionID:   sessID,
		PID:         int(pid.Int64),
		ExitCode:    int(exitCode.Int64),
		NextAction:  nextAction(hasDone, agentStatus, messageID),
	}
	d.addProgressDetail(&res, parentID)
	return res, terminal, nil
}

// addProgressDetail fills in what the child has actually been doing. The
// caller can't see the child's own session, so this is the only window into
// it — best-effort, never fails the status call.
func (d *Daemon) addProgressDetail(res *TaskStatusResult, parentID int64) {
	var toProject string
	var ts time.Time
	if err := d.Store.db.QueryRow(
		`SELECT IFNULL(to_project,''), ts FROM messages WHERE id=?`, parentID).
		Scan(&toProject, &ts); err == nil {
		res.Project = toProject
		if !ts.IsZero() {
			res.ElapsedSecs = int64(time.Since(ts).Seconds())
		}
	}

	todos, err := d.Todos.List(parentID)
	if err == nil {
		res.TodosTotal = len(todos)
		for _, t := range todos {
			if t.State == "done" {
				res.TodosDone++
			} else if res.CurrentStep == "" {
				res.CurrentStep = t.Content
			}
			res.Todos = append(res.Todos, TodoLine{Content: t.Content, State: t.State})
		}
	}

	// Latest reply on the thread — the child's own words, if it posted any.
	var last string
	if err := d.Store.db.QueryRow(
		`SELECT content FROM messages WHERE thread_id=? ORDER BY id DESC LIMIT 1`,
		parentID).Scan(&last); err == nil {
		res.LastUpdate = firstLine(last, 200)
	}
	// Latest self-report from the child, with how long ago it said it. An ETA
	// is only meaningful next to its age.
	var note string
	var noteTS time.Time
	if err := d.Store.db.QueryRow(
		`SELECT content, ts FROM messages WHERE thread_id=? AND status='progress'
		 ORDER BY id DESC LIMIT 1`, parentID).Scan(&note, &noteTS); err == nil {
		res.ProgressNote = firstLine(note, 200)
		res.ProgressAgeSecs = int64(time.Since(noteTS).Seconds())
		if m := etaPattern.FindStringSubmatch(note); m != nil {
			fmt.Sscanf(m[1], "%d", &res.ETAMinutes)
		}
	}
}

// firstLine trims a message to its first line, capped at max runes.
func firstLine(s string, max int) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > max {
		return string(r[:max-1]) + "…"
	}
	return s
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
		`SELECT IFNULL(s.name,'-'), p.name, s.status, IFNULL(s.opencode_session_id,'') AS session_id, IFNULL(s.root_thread_id,0) AS parent_id, IFNULL(s.created_at,''), IFNULL(s.finished_at,''),
		        IFNULL(m.from_project,''), IFNULL(s.pid,0), p.path
		 FROM sessions s JOIN projects p ON s.project_id=p.id
		 LEFT JOIN messages m ON m.id = s.task_msg_id
		 ORDER BY s.id DESC`)
	if err != nil {
		return errResp(req.ID, err.Error())
	}
	defer rows.Close()
	var out []SessionListItem
	for rows.Next() {
		var it SessionListItem
		if err := rows.Scan(&it.Name, &it.Project, &it.Status, &it.AgentSessionID, &it.ParentID, &it.CreatedAt, &it.FinishedAt, &it.EngagedBy, &it.PID, &it.Dir); err != nil {
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

type ReportProgressParams struct {
	ThreadID   int64  `json:"thread_id"`
	From       string `json:"from"`
	Note       string `json:"note"`
	ETAMinutes int    `json:"eta_minutes"`
}

// handleReportProgress records a working child's status update. Posted with
// status "progress", which never wakes anyone: it is the child narrating, and
// spawning the parent to hear it would cost a whole agent process.
func (d *Daemon) handleReportProgress(req protocol.Request) protocol.Response {
	var p ReportProgressParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return errResp(req.ID, "bad params: "+err.Error())
	}
	if p.ThreadID == 0 {
		return errResp(req.ID, "thread_id required")
	}
	if strings.TrimSpace(p.Note) == "" {
		return errResp(req.ID, "note required")
	}
	root, err := d.Comms.GetMessage(p.ThreadID)
	if err != nil {
		return errResp(req.ID, "thread "+err.Error())
	}
	threadID := p.ThreadID
	if root.ThreadID != 0 {
		threadID = root.ThreadID
		root, _ = d.Comms.GetMessage(threadID)
	}
	from := p.From
	if from == "" {
		from = root.ToProject
	}
	content := p.Note
	if p.ETAMinutes > 0 {
		content = fmt.Sprintf("%s (eta ~%dmin)", p.Note, p.ETAMinutes)
	}
	msg, err := d.Comms.PostMessage(root.ChannelID, threadID, from, root.FromProject, content, "progress")
	if err != nil {
		return errResp(req.ID, err.Error())
	}
	result, _ := json.Marshal(PostResult{MessageID: msg.ID})
	return protocol.Response{ID: req.ID, Result: result}
}

// etaPattern pulls the minutes back out of a progress note's "(eta ~12min)".
var etaPattern = regexp.MustCompile(`\(eta ~(\d+)min\)`)

// handlePrune clears task history. Sessions still running are left alone, so a
// prune during live work cannot orphan an agent mid-task.
func (d *Daemon) handleThreadDelete(req protocol.Request) protocol.Response {
	var p struct {
		ThreadID int64 `json:"thread_id"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return errResp(req.ID, "bad params: "+err.Error())
	}
	if p.ThreadID == 0 {
		return errResp(req.ID, "thread_id required")
	}
	res, err := d.Store.DeleteThread(p.ThreadID)
	if err != nil {
		return errResp(req.ID, err.Error())
	}
	result, _ := json.Marshal(res)
	return protocol.Response{ID: req.ID, Result: result}
}

func (d *Daemon) handlePrune(req protocol.Request) protocol.Response {
	var p struct {
		OlderThanHours float64 `json:"older_than_hours"`
	}
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return errResp(req.ID, err.Error())
		}
	}
	res, err := d.Store.Prune(time.Duration(p.OlderThanHours * float64(time.Hour)))
	if err != nil {
		return errResp(req.ID, err.Error())
	}
	result, _ := json.Marshal(res)
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
	case "todo_delete":
		if p.ID == 0 {
			return errResp(req.ID, "todo_delete needs id")
		}
		if err := d.Todos.Delete(p.ID); err != nil {
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

// reconcileOrphanedSessions marks "active" sessions with a dead PID as
// exited/failed. A daemon restart loses the goroutine watching the spawned
// process, so its real exit is never observed — the row stays "active"
// forever and silently blocks future wakes on that thread. Run at startup.
func (d *Daemon) reconcileOrphanedSessions() {
	rows, err := d.Store.db.Query(`
		SELECT s.id, s.pid, s.root_thread_id, m.channel_id, m.from_project, m.to_project
		FROM sessions s JOIN messages m ON m.id = s.root_thread_id
		WHERE s.status = 'active'`)
	if err != nil {
		log.Printf("reconcile: query active sessions: %v", err)
		return
	}
	type orphanCandidate struct {
		id, rootThreadID, channelID int64
		pid                         sql.NullInt64
		fromProject, toProject      string
	}
	var candidates []orphanCandidate
	for rows.Next() {
		var c orphanCandidate
		if err := rows.Scan(&c.id, &c.pid, &c.rootThreadID, &c.channelID, &c.fromProject, &c.toProject); err != nil {
			log.Printf("reconcile: scan: %v", err)
			continue
		}
		candidates = append(candidates, c)
	}
	if err := rows.Err(); err != nil {
		log.Printf("reconcile: rows: %v", err)
	}
	rows.Close()

	for _, c := range candidates {
		if c.pid.Valid && c.pid.Int64 > 0 && processAlive(c.pid.Int64) {
			continue // still genuinely running
		}
		var doneCount int
		d.Store.db.QueryRow(
			`SELECT count(*) FROM messages WHERE thread_id=? AND status='done'`, c.rootThreadID).Scan(&doneCount)
		code := 0
		if doneCount == 0 {
			code = -1
		}
		if err := d.Sessions.MarkExited(c.id, code); err != nil {
			log.Printf("reconcile: mark session %d exited: %v", c.id, err)
			continue
		}
		log.Printf("reconcile: orphaned session %d (pid=%v) had no live process at startup, marked exited (done=%t)", c.id, c.pid, doneCount > 0)
		if d.SafetyNetEnabled && doneCount == 0 {
			content := "BLOCKED: agent process was orphaned by a daemon restart and never posted a done reply"
			if _, err := d.Comms.PostMessage(c.channelID, c.rootThreadID, c.toProject, c.fromProject, content, "done"); err != nil {
				log.Printf("reconcile: synthetic done reply for thread %d: %v", c.rootThreadID, err)
			}
		}
	}
}

// processAlive reports whether pid names a live process this OS user can
// signal. Signal 0 sends nothing — it only probes existence/permission.
func processAlive(pid int64) bool {
	return syscall.Kill(int(pid), 0) == nil
}

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
	d.reconcileOrphanedSessions()
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
