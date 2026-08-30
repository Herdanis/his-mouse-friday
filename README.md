# his-mouse-friday

Per-directory AI agent orchestration harness. Each repo gets a dedicated AI
agent "engineer" that owns it. When work crosses a dependency boundary, an
agent engages the other repo's agent via a shared comms layer instead of
editing foreign code directly.

## Prerequisites

- **Go 1.26+** — the installer fetches it for you if missing
- **opencode** — the agent runtime (spawned per task)
- **python3** — used by the protection plugin for daemon socket calls
- **sqlite3 CLI** — used by the protection plugin to read the project
  registry directly (faster than a daemon round-trip). Optional: falls back
  to a daemon socket call if missing, just slower.
- **curl** — used by `scripts/install.sh` to fetch Go and the plugin/command
  files. Install-time only, not needed at runtime.
- **macOS**, optional — `hmf watch` fires a desktop notification via
  `osascript` (built into macOS, nothing extra to install) when a watched
  task finishes. On other platforms `hmf watch` still works, it just skips
  the notification.

Verify:

    opencode --version
    python3 --version
    sqlite3 --version
    curl --version

## Install

One-liner (macOS / Linux) — fetches Go if missing, `go install`s `hmf` +
`hmf-mcp`, drops the opencode plugin + `/hmf-setup` + `/hmf-register` slash
commands into `~/.config/opencode/`:

    curl -fsSL https://raw.githubusercontent.com/Herdanis/his-mouse-friday/main/install.sh | bash

Restart your shell after (Go bin dir on `PATH`), then wire the MCP server into
`~/.config/opencode/opencode.json`:

    {
      "$schema": "https://opencode.ai/config.json",
      "mcp": {
        "hmf": {
          "type": "local",
          "command": ["hmf-mcp"],
          "enabled": true
        }
      }
    }

Start the daemon:

    hmf up

`OPENCODE_CONFIG` overrides the config dir (default `~/.config/opencode`).

**Dev build** (from a clone):

    git clone git@github.com:Herdanis/his-mouse-friday.git
    cd his-mouse-friday
    make build
    go install ./cmd/...

## Uninstall

    curl -fsSL https://raw.githubusercontent.com/Herdanis/his-mouse-friday/main/uninstall.sh | bash

Removes the plugin, slash commands, and the `hmf` / `hmf-mcp` binaries. Prints
a reminder to drop the `hmf` block from `opencode.json` (the installer won't
edit your config file).

## Wire the MCP server into opencode

`hmf-mcp` exposes the 5 orchestration tools to opencode agents. The install
step prints the snippet above; add it to `~/.config/opencode/opencode.json`
(global) or a repo's `opencode.json`. See `examples/opencode.json` for a
version with bash permission rules too.

## Setup

**Option A — Slash command (recommended):**

After `make install`, in any opencode session:

- `/hmf-setup` — full guided setup (workspace, project, config files, tailored MOUSE.md)
- `/hmf-register` — fast registration (one question, minimal config)

**Option B — Manual:**

1. Start the daemon (foreground, separate terminal):

       hmf up

2. Register a workspace + your repos:

       hmf workspace add companyA
       hmf project add payment-service ~/code/payment --workspace companyA
       hmf project add user-service ~/code/user-service --workspace companyA
       hmf status

3. Add `mouse.yaml` + `MOUSE.md` to each repo (see `examples/`).
   Set `a2a.allow_inbound: true` on repos other agents may engage.
   `mouse.yaml` declares command + filesystem permissions (gitignore-style:
   deny / ask / allow). `/hmf-setup` auto-generates `opencode.json` from it,
   so opencode enforces the policy natively — agents physically cannot run
   denied commands (e.g. `kubectl delete`, `gcloud * delete`).

4. Open opencode in a registered repo. The agent now has 5 tools:
   `engage_project_agent`, `post_message`, `read_channel`, `read_thread`,
   `list_project_agents`. The `from` field is auto-detected from cwd.

## Config (per repo)

- `MOUSE.md` — agent runbook/guide
- `mouse.yaml` — agent binary, model, permissions, a2a
- `AGENTS.md` — OpenCode instruction file (references MOUSE.md)

See `examples/`.

## Waiting on a task without polling

An AI orchestrator polling `task_status` in a loop costs tokens on every
check. If a human is the one actually waiting, skip that entirely:

```bash
hmf watch <message_id>
```

Blocks in its own terminal (zero LLM cost — it's a plain CLI loop, not an
agent), checks the daemon every ~2min, and fires a macOS desktop
notification the moment the task's `done` reply lands (or if it ends
without one). Get the `message_id` from whatever posted the task
(`post_message`'s return value, or `hmf task list` / `hmf session list`).

## Troubleshooting

**A session shows `active` in `hmf session list` but nothing's happening.**
Usually a daemon restart orphaned it — the goroutine watching that spawned
process died with the old daemon, so its real exit was never observed. Fix:

```bash
hmf down && hmf up
```

Every startup runs `reconcileOrphanedSessions()`: any `active` session whose
PID is no longer alive gets marked `exited` (if a done reply already landed)
or `failed` + a synthetic `BLOCKED` reply (if not). Doesn't disrupt other
live agents — they don't hold a persistent daemon connection.

Manual fix without restarting, if needed:

```bash
# find it
sqlite3 ~/.hmf/hmf.db "SELECT id,pid,root_thread_id FROM sessions WHERE status='active';"
# confirm it's actually dead
ps -p <pid>
# dead + done reply already exists for its thread → exited
sqlite3 ~/.hmf/hmf.db "UPDATE sessions SET status='exited', exit_code=0 WHERE id=<id>;"
# dead + no done reply → failed (redispatch the task)
sqlite3 ~/.hmf/hmf.db "UPDATE sessions SET status='failed', exit_code=-1 WHERE id=<id>;"
```

## TODOs

- [x] **Enforce edit protection for registered projects from unregistered dirs.**
  Fixed via `examples/plugins/hmf/plugin.ts` — an opencode plugin that
  intercepts `edit` and `bash` calls and checks target paths against the hmf
  registry (edit) plus path args extracted from the command (bash: `terraform
  -chdir`, `docker compose -f`, `bash script.sh`, ...). Blocks only when the
  target resolves inside a *registered* project other than the caller's own;
  unregistered paths are untouched.

- [ ] **Fix auto-spawn reliability.** Engaged agent sometimes doesn't post back
  (`in_progress`/`done`) when auto-spawned by `engage_project_agent` vs
  running `opencode run` manually. Likely prompt-engineering — the task
  prompt needs explicit instructions to use hmf tools.

- [x] **Thread `to` field through `post_message` auto-fill.** Fixed in
  `internal/daemon/daemon.go:handlePost` — a reply that omits `to` now
  resolves it from the thread root (the other party relative to `from`),
  skipped for `status=done` to avoid waking the originator back on a
  worker's completion notice.

- [ ] **Session resume.** User should be able to reopen any spawned agent's
  session (`hmf session attach <id>`) and continue directly.

- [ ] **Real multi-agent scenario.** 3+ agents coordinating (payment →
  user-service → frontend) to prove the full team model.
