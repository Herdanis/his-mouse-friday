# AGENTS.md

Guidance for AI agents working in this repository.

## Project

`his-mouse-friday` (hmf) is a per-directory AI agent orchestration harness. A
long-running Go daemon brokers agent-to-agent (A2A) communication over a
unix socket. A stateless MCP shim (`hmf-mcp`, stdio) exposes 6 tools to
opencode sessions; CLI (`hmf`) manages workspaces, projects, and the daemon
lifecycle. Each registered repo declares its own `mouse.yaml` (permissions,
agent runtime) and `MOUSE.md` (runbook); `AGENTS.md` points opencode at
`MOUSE.md`.

## Conventions

- **Per-directory config is authoritative locally.** A sub-agent spawned into
  a directory reads that directory's `MOUSE.md`/`mouse.yaml`/`AGENTS.md` before
  acting and treats them as the guide for decisions scoped to that directory.
  Do not override directory rules from the orchestrator unless a global
  guardrail requires it.
- **Orchestrator-spawn model.** The root agent spawns sub-agents for
  directory-scoped work. A sub-agent operates within the permissions its
  spawner granted plus the rules its target directory declares.
- **Config lives where it applies.** Global concerns at root; directory rules
  in their own directories. Do not centralize directory rules at the root.
- **Agents own their repo.** Do NOT edit, write to, or run another registered
  project's files directly — engage that project's agent via `post_message`
  with a `to` field. Reading it is allowed, so you can verify work you asked
  for. The hmf MCP `list_project_agents` tool is the registry of who owns
  what.
- **Prefer native OpenCode mechanisms** (`opencode.json`, per-directory
  instruction files, `.opencode/`) over custom config formats unless the
  harness explicitly defines one (`mouse.yaml` is the harness-defined format).

## Build / test / lint

    go install ./cmd/hmf          # CLI binary → $(go env GOPATH)/bin/hmf
    go install ./cmd/hmf-mcp      # MCP shim  → .../hmf-mcp
    go build ./...                # compile-check without installing
    go test ./...                 # all packages (no test files under cmd/)
    go test ./internal/daemon -run TestHandle_PostToGeneralWakesAgent -v   # single test

- No CGO required (`modernc.org/sqlite` is pure-Go). No special env setup.
- `lefthook` pre-commit runs `go vet ./...`, `go test ./...`, and
  `gitleaks protect --staged` in parallel (see `lefthook.yml`). `go vet` and
  `go test` only fire on `*.go` globs.
- Commit style: **Conventional Commits** (e.g. `feat(hmf):`, `fix:`,
  `test(hmf):`, `refactor(hmf):`, `docs:`). Scope is usually `hmf`.

## Architecture

Two binaries over one daemon:

- `cmd/hmf` → `internal/cli` (cobra). Subcommands: `up`, `down`, `status`,
  `workspace add|list|delete`, `project add|list|delete`, `init`, `config show`,
  `done`. `hmf up` blocks (foreground daemon); run in a separate terminal.
- `cmd/hmf-mcp` → `internal/mcp.RunServer` (stdio MCP server via
  `modelcontextprotocol/go-sdk`). Stateless: every tool call is forwarded to
  the daemon over the unix socket.
- `internal/daemon` — `Daemon` (request router), `Store` (sqlite via
  modernc.org/sqlite), `Registry` (workspaces/projects), `Sessions` (spawned
  agent lifecycle), `Comms` (channels/threads/messages), `Launcher` (spawns
  `opencode run` with env vars).
- `internal/protocol` — JSON-over-socket wire format (`Request{Method,Params,ID}`,
  `Response{ID,Result,Error}`) plus `StateDir()`/`SocketPath()`/`DBPath()`.
- `internal/config` — `MouseConfig` (yaml) + `ResolveMouse(repoPath)` (falls
  back to `~/.hmf/mouse.yaml` for unregistered dirs).

State lives under `~/.hmf/` (`daemon.sock`, `hmf.db`, global `mouse.yaml`,
`hmf.log`).

`internal/daemon/logging.go` owns the event log — **one file, `~/.hmf/hmf.log`**,
for daemon events, agent stdout/stderr and backgrounded daemon stderr. Don't add
a second log file; the old `capture.log` and installer `daemon.log` were merged
into this one because three files meant no single place to look.

- `logf(event, format, ...)` / `logErrf(...)` — the latter stamps `ERROR`, so
  `grep ERROR` is the complete failure list. Use it for every failure path.
- Correlation: every post/wake/spawn/exit line carries `thread=<root id>`
  (`threadKey(threadID, msgID)` when a message may be a root), plus
  `session=` / `msg=` where known. `grep thread=N` must replay a whole task —
  keep new lines to that shape.
- Read-only methods listed in `quietMethods` log a summary, not bodies: the
  monitor TUI polls them and full JSON pushed real events out of the file.
- `LogWriter()` is the io.Writer `hmf up` hands to stdlib `log` and the launcher
  hands to the child process. Writes swallow their errors on purpose — logging
  must never fail a caller.
- 40MB cap, rotates to `hmf.log.1` (one backup).

Debugging an agent that never replied starts with `tail -f ~/.hmf/hmf.log`, not
with reading the DB.
Tests override via `HMF_STATE_DIR` (temp dir) — required because macOS socket
path length is capped; test helpers `spinDaemon`/`spinCLIDaemon` set this up.

## MCP tools (exposed to opencode)

`post_message`, `task_status`, `report_progress`, `read_channel`,
`read_thread`, `list_project_agents`. `post_message` with a `to` field and no `thread_id` is
a thread root that **wakes** the addressed project's agent (spawns
`opencode run` with `HMF_CHANNEL_ID`, `HMF_TASK_MSG_ID`, `HMF_PROJECT`,
`HMF_FROM` env vars set). Replies set `thread_id` and do not wake.
`report_progress` lets a working child say where it is and how much longer it
needs; it posts with status `progress` and never wakes anyone, so a status
update costs the parent nothing. `task_status` returns it as `progress_note` /
`eta_minutes` / `progress_age_secs` — read the ETA against the age, since the
first two are the child's claims and the third is fact.

`hmf done [summary]` posts a `done` reply — only valid inside a spawned
agent session (`HMF_CHANNEL_ID` must be set).

## Repo style

- **Comments:** section/block comments use the 44-char `=` banner format (see
  `lefthook.yml`, daemon files). Inline comments minimal — only
  non-obvious "why" and gotchas. No install/usage prose in code; that lives in
  `README.md` / `docs/`.
- **Modes:** this repo runs in **caveman (full)** + **ponytail (full)** by
  default — terse prose, shortest working diff, stdlib first. See
  `~/.config/opencode/AGENTS.md` for the full ruleset. Security warnings and
  irreversible actions revert to normal prose.
- File layout: `cmd/<bin>/main.go` thin entrypoints; all logic in
  `internal/<package>/`. Test files alongside source as `*_test.go`. Shared
  test helpers (e.g. `spinDaemon`) live in the package's `*_test.go`.

## OpenCode config at root (`opencode.json`)

- `edit`, `read`, `glob`, `grep` → `allow`; `bash` is `{"*": "ask"}` plus one
  `"deny"` entry per pattern in `mouse.yaml`'s `commands.deny`. The `ask`
  catch-all does NOT hang headless spawns: the daemon passes `--auto`
  (`internal/daemon/runtime_opencode.go`), which auto-approves anything not
  explicitly denied. Do not "fix" it to a blanket `allow` — a security review
  already flagged that.
- Do not hand-edit the `permission.bash` map. `hmf sync` regenerates it from
  `mouse.yaml` (the source of truth); hand edits drift and get overwritten.
  `commands.ask` is deliberately not mirrored — the plugin
  (`examples/plugins/hmf/plugin.ts`) enforces that tier.
- `permissions.fs` is plugin-only: native `edit` is a bare string with no path
  patterns, so nothing can mirror it. The plugin checks fs globs against edit
  targets and against paths named in bash commands, and falls back to
  `~/.hmf/mouse.yaml` when a directory has no `mouse.yaml` of its own.
  Plugin tests: `bun test examples/plugins/hmf/` (wired into `lefthook.yml`).
- Every mouse.yaml guardrail is an opencode plugin. `provider: claude` spawns
  `claude -p`, which loads no plugin and therefore enforces none of it.
- `.opencode/` is machine-managed runtime state (goals, sessions, locks).
  Do not hand-edit.

## Working in this repo

- Before editing a path, check whether it falls inside a *different* hmf
  registered project (`list_project_agents`). If yes and your cwd is outside
  that project, do NOT edit directly — engage that project's agent instead.
  Reading it is fine.
- Spawning a sub-agent for directory-scoped work? Use `opencode run` and pass
  the hmf env vars the launcher sets (`HMF_CHANNEL_ID`, `HMF_TASK_MSG_ID`,
  `HMF_PROJECT`, `HMF_FROM`) so the child can call `hmf done` and post
  replies on the right thread.
- **Delegate intent, not a diff.** Don't grep/read inside the target project
  before delegating — state the goal and constraints and let its agent locate
  the code. Pre-researching file paths and line numbers spends your context on
  work the child is about to redo anyway, and the numbers are stale the moment
  the child edits anything.
- **Read across projects is allowed; writing is not.** A parent may open any
  registered project read-only — `read`/`grep`/`glob`, `cat`, `ls`, `rg`,
  `git log`/`status`/`diff` — to check that a child's work matches what was
  asked. Editing, writing, or running that project (tests, builds, git
  commands that change state) stays with its own agent: delegate with
  `post_message`. The plugin enforces this; anything outside its read-only
  allowlist is treated as a write and blocked.
- **Start with the done reply, verify only when it matters.** The spawned agent
  reports which files it changed and whether the project's verify commands
  passed. Re-reading everything it touched just re-acquires context the child
  already holds — read the specific files whose correctness you actually need
  to confirm, or `post_message` a follow-up on the same thread, where the child
  still has the context and answers cheaply.
- **Polling a spawned agent.** Prefer `task_status(message_id, wait_seconds=120)`
  over a manual sleep loop — the daemon blocks server-side (cheap local
  polling, zero LLM tokens) and returns the moment `has_done` flips or the
  agent reaches a terminal state, capped at 120s (2min). Cap is 120, not
  higher: some MCP clients (opencode, observed) impose their own tool-call
  timeout and error with `-32001 Request timed out` on a longer wait —
  passing `wait_seconds` above 120 doesn't get you a longer wait, it risks
  the call erroring instead of returning. One call replaces a
  sleep→check→sleep→check round; if it returns with `has_done` still false,
  just call again with the same `message_id` — every call blocks for its full
  wait, so the loop paces itself and needs no `sleep` between calls. (Needs a recent
  `hmf-mcp` — MCP tool schemas are negotiated once at connection time, so a
  session already running when `hmf-mcp` was rebuilt won't see
  `wait_seconds` until it reconnects. Fall back to a manual sleep-and-recheck
  loop only if the tool schema doesn't show it. Also: a *resumed* opencode
  session that's been running long enough to have its own `sleep`-then-check
  precedent baked into its conversation history may keep imitating that
  pattern even once `wait_seconds` is available — that's in-context habit,
  not a missing capability; a fresh conversation is the real fix, not more
  instructions.)

  Once dispatched, don't self-initiate a verification loop unless the task
  actually needs the result before you can continue — `post_message` (with
  `to` set) is fire-and-forget: it spawns the target project's agent and
  returns immediately, it does not wait for that agent to finish. If nothing
  in your current turn depends on the outcome, dispatch and stop; check later
  only when asked.

  Either way, `has_done` is the completion signal — check it FIRST, and stop
  the moment it's `true`. Do NOT wait for `agent_status == exited` as the
  primary signal: a resumed session (`opencode run -s`) doesn't self-terminate
  after finishing a turn — it posts its done reply and then just sits there
  alive until hmf's watchdog reaps it, up to 60min later (`HMF_WATCHDOG`).
  Waiting on `exited` after `has_done` is already true means waiting on a
  timer, not on the agent.

  `agent_status == failed` does NOT mean the task failed if `has_done` is
  already `true` — it only means the process's raw exit code was nonzero
  (e.g. someone killed an already-finished resumed session). Since the fix in
  `daemon.go`'s `OnExit`, a done reply landing before exit forces the status
  to `exited`; older sessions or races can still show `failed` alongside
  `has_done: true`. Never redispatch a task because `agent_status` looks bad
  while `has_done` is `true` — that just repeats already-completed work. Only
  redispatch when `has_done` is still `false` AND `agent_status` is
  `exited`/`failed`/`no_agent` (safety-net BLOCKED message should already be
  posted in that case). Real tasks (migration + codegen + build) run several
  minutes — one short poll timing out is not evidence the agent is stuck.
- **`status='active'` in the DB is a claim, not a fact.** The exit watcher is a
  goroutine; a daemon restart loses it and the row stays `active` forever,
  blocking deletes, prunes and wakes. `Store.ReapDeadSessions` (pid-liveness
  check) runs before delete/prune; rows with pid 0/NULL are left alone because
  `Sessions.Create` precedes `SetPID`.
- **Thread-scoped session queries need a project scope too.** Several projects
  legitimately work one thread at once. Both the resume lookup and
  `threadSessionActive` filter on `root_thread_id` AND the project — dropping
  either one lets project B resume project A's opencode session id (dead in
  B's directory: the agent hangs and never replies) or swallows B's wake
  entirely. Same class of bug twice; keep the scope.
- Known open issues in `README.md` TODOs: auto-spawn reliability, session
  resume, real 3+ agent scenario. Edit-protection and `to` field auto-fill on
  replies are fixed — see `README.md` TODOs for details. Don't claim the
  remaining ones are solved.
