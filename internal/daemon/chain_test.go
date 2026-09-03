package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/herdanis/his-mouse-friday/internal/protocol"
)

// ============================================
// Three-project chain: A → B → C on one thread
// ============================================

const chainMouse = "agent:\n  primary:\n    provider: opencode\na2a:\n  allow_inbound: true\n  allow_outbound: true\n"

// awaitCapturedSessionID blocks until project's newest session carries want.
// Capture runs in a goroutine, and the next hop's resume lookup only proves
// anything once the previous hop's runtime session id has landed.
func awaitCapturedSessionID(t *testing.T, d *Daemon, project, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		var got sql.NullString
		d.Store.db.QueryRow(
			`SELECT opencode_session_id FROM sessions s JOIN projects p ON s.project_id=p.id
			 WHERE p.name=? ORDER BY s.id DESC LIMIT 1`, project).Scan(&got)
		if got.String == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s agent session id = %q, want %q (capture never landed)", project, got.String, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestChain_ThreeProjectDelegation walks the full A2A shape the harness
// exists for: a human dispatches to svc-a, svc-a engages svc-b, svc-b engages
// svc-c, all three reply done on the one thread.
func TestChain_ThreeProjectDelegation(t *testing.T) {
	d := setupDaemon(t)
	d.Registry.AddWorkspace("companyA")
	dirs := map[string]string{}
	for _, name := range []string{"svc-a", "svc-b", "svc-c"} {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "mouse.yaml"), []byte(chainMouse), 0644)
		d.Registry.AddProject("companyA", name, dir)
		dirs[name] = dir
	}
	var spawns []SpawnConfig
	d.Launcher = &Launcher{SpawnFn: func(cfg SpawnConfig) (int, error) {
		spawns = append(spawns, cfg)
		return 1000 + len(spawns), nil
	}}
	// A distinct runtime session id per project: if the resume lookup ever
	// widens back to root_thread_id alone, B or C picks up A's id here.
	d.CaptureAgentSessionID = func(cfg SpawnConfig) (string, error) { return "ses-" + cfg.ProjectID, nil }

	post := func(id int64, p map[string]any) int64 {
		t.Helper()
		params, _ := json.Marshal(p)
		resp := d.Handle(context.Background(), protocol.Request{Method: "post_message", Params: params, ID: id})
		if resp.Error != nil {
			t.Fatalf("post %v: %s", p["content"], resp.Error.Message)
		}
		var pr PostResult
		json.Unmarshal(resp.Result, &pr)
		return pr.MessageID
	}

	// Resume is scoped to root+project+binary, so every hop is that project's
	// first session on the thread and must spawn fresh. Checked per hop, not
	// at the end: a leaked id also skips capture, hiding behind a later assert.
	freshSpawn := func(name string) {
		t.Helper()
		if last := spawns[len(spawns)-1]; last.AgentSessionID != "" {
			t.Fatalf("%s spawn resumed %q — a sibling project's session id is dead in its dir", name, last.AgentSessionID)
		}
	}

	root := post(1, map[string]any{"from": "dir:lab", "to": "companyA/svc-a", "content": "ship the feature"})
	freshSpawn("svc-a")
	awaitCapturedSessionID(t, d, "svc-a", "ses-companyA/svc-a")
	post(2, map[string]any{"thread_id": root, "from": "companyA/svc-a", "to": "companyA/svc-b", "content": "svc-b half"})
	freshSpawn("svc-b")
	awaitCapturedSessionID(t, d, "svc-b", "ses-companyA/svc-b")
	post(3, map[string]any{"thread_id": root, "from": "companyA/svc-b", "to": "companyA/svc-c", "content": "svc-c half"})
	freshSpawn("svc-c")
	awaitCapturedSessionID(t, d, "svc-c", "ses-companyA/svc-c")

	chain := []string{"svc-a", "svc-b", "svc-c"}
	if len(spawns) != 3 {
		t.Fatalf("spawns = %d, want 3 (one per hop)", len(spawns))
	}
	for i, name := range chain {
		if spawns[i].Dir != dirs[name] {
			t.Errorf("spawn %d ran in %q, want %s dir %q", i, spawns[i].Dir, name, dirs[name])
		}
		if spawns[i].TaskMsgID != root {
			t.Errorf("spawn %d TaskMsgID=%d, want shared root %d", i, spawns[i].TaskMsgID, root)
		}
	}

	// One session per project, all bound to the one root thread.
	for _, name := range chain {
		var n int
		d.Store.db.QueryRow(
			`SELECT count(*) FROM sessions s JOIN projects p ON s.project_id=p.id
			 WHERE p.name=? AND s.root_thread_id=?`, name, root).Scan(&n)
		if n != 1 {
			t.Errorf("%s sessions on root %d = %d, want 1", name, root, n)
		}
	}

	// session_list is what `hmf monitor` renders: engaged_by must recover the
	// chain shape that the shared root_thread_id alone flattens away.
	resp := d.Handle(context.Background(), protocol.Request{Method: "session_list", ID: 4})
	if resp.Error != nil {
		t.Fatalf("session_list: %s", resp.Error.Message)
	}
	var items []SessionListItem
	json.Unmarshal(resp.Result, &items)
	if len(items) != 3 {
		t.Fatalf("session_list = %d rows, want 3", len(items))
	}
	engagedBy := map[string]string{}
	sessID := map[string]int64{}
	engagedBySession := map[string]int64{}
	for _, it := range items {
		engagedBy[it.Project] = it.EngagedBy
		sessID[it.Project] = it.ID
		engagedBySession[it.Project] = it.EngagedBySession
		if it.ParentID != root {
			t.Errorf("%s parent_id=%d, want root %d", it.Project, it.ParentID, root)
		}
	}
	want := map[string]string{
		"svc-a": "dir:lab",
		"svc-b": "companyA/svc-a",
		"svc-c": "companyA/svc-b",
	}
	for proj, from := range want {
		if engagedBy[proj] != from {
			t.Errorf("%s engaged_by=%q, want %q", proj, engagedBy[proj], from)
		}
	}
	// engaged_by narrowed to the exact session: the monitor nests each agent
	// under the session that engaged it, and a project name cannot tell two
	// sessions of one project apart.
	if engagedBySession["svc-a"] != 0 {
		t.Errorf("svc-a engaged_by_session=%d, want 0 — a human dispatcher owns no session",
			engagedBySession["svc-a"])
	}
	if engagedBySession["svc-b"] != sessID["svc-a"] {
		t.Errorf("svc-b engaged_by_session=%d, want svc-a's session %d",
			engagedBySession["svc-b"], sessID["svc-a"])
	}
	if engagedBySession["svc-c"] != sessID["svc-b"] {
		t.Errorf("svc-c engaged_by_session=%d, want svc-b's session %d",
			engagedBySession["svc-c"], sessID["svc-b"])
	}

	// Each replies done on the shared root, deepest first.
	for i, name := range []string{"svc-c", "svc-b", "svc-a"} {
		post(int64(10+i), map[string]any{
			"thread_id": root, "from": "companyA/" + name,
			"content": name + " finished", "status": "done",
		})
	}
	// A done never wakes anyone — three replies must not spawn a fourth agent.
	if len(spawns) != 3 {
		t.Errorf("spawns = %d after the done replies, want 3", len(spawns))
	}

	tp, _ := json.Marshal(map[string]any{"message_id": root})
	tresp := d.Handle(context.Background(), protocol.Request{Method: "task_status", Params: tp, ID: 20})
	if tresp.Error != nil {
		t.Fatalf("task_status: %s", tresp.Error.Message)
	}
	var ts TaskStatusResult
	json.Unmarshal(tresp.Result, &ts)
	if !ts.HasDone {
		t.Error("task_status(root).has_done = false after every hop replied done")
	}
	if ts.Project != "companyA/svc-a" {
		t.Errorf("task_status project = %q, want the dispatched project", ts.Project)
	}
	if ts.LastUpdate != "svc-a finished" {
		t.Errorf("last_update = %q, want the newest reply on the thread", ts.LastUpdate)
	}

	// read_thread carries the whole conversation: root + 2 delegations +
	// 3 acks + 3 dones.
	rp, _ := json.Marshal(map[string]any{"message_id": root})
	rresp := d.Handle(context.Background(), protocol.Request{Method: "read_thread", Params: rp, ID: 21})
	var thread []Message
	json.Unmarshal(rresp.Result, &thread)
	var acks int
	for _, m := range thread {
		if m.Status == "ack" {
			acks++
		}
	}
	if len(thread) != 9 || acks != 3 {
		t.Errorf("thread = %d messages (%d acks), want 9 with one ack per hop", len(thread), acks)
	}
}
