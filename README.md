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

**`timeout` is required, not optional:**

```json
"mcp": {
  "hmf": {
    "command": ["hmf-mcp"],
    "enabled": true,
    "type": "local",
    "timeout": 330000
  }
}
```

`task_status` blocks server-side for up to 5 minutes while a delegated agent
works — that's deliberate, it's what paces polling instead of burning tokens
on a sleep-and-recheck loop. opencode's MCP client defaults to a **5 second**
request timeout, so without this setting every `task_status` call dies with:

    MCP error -32001: Request timed out

330000 ms (5.5 min) leaves headroom over the daemon's 5 min wait.

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
   Set `a2a.allow_inbound: true` on repos other agents may engage, and
   `a2a.allow_outbound: true` on repos whose agent may delegate to others.
   Both are enforced and both default to false when omitted, so a repo that
   declares neither can neither be engaged nor delegate.
   `mouse.yaml` declares command + filesystem permissions (gitignore-style:
   deny / ask / allow) and is the single source of truth for them.
   `/hmf-setup` writes a baseline `opencode.json`; `hmf sync` then mirrors
   `mouse.yaml`'s `commands.deny` into its `permission.bash` map, so opencode
   also enforces the deny tier natively — a backstop for the case where the
   plugin fails to load. Re-run `hmf sync` after editing the deny list.
   `commands.ask` is not mirrored: it is plugin-enforced only, because the
   daemon spawns agents with `--auto` and a native `ask` is auto-approved
   there (and in a TTY it would prompt only for the plugin to throw anyway).
   `permissions.fs` is plugin-enforced only too — opencode's native `edit`
   permission is a plain `allow`/`ask`/`deny` string with no path patterns, so
   there is nothing to mirror it into. The plugin matches fs patterns
   gitignore-style (no-slash patterns match at any depth, `*` stops at `/`,
   a trailing `/` covers a directory's contents) against both edit targets and
   paths named in a bash command.

   In a directory with no `mouse.yaml`, the plugin falls back to
   `~/.hmf/mouse.yaml` (same rule as the daemon's `config.ResolveMouse`), so
   the global defaults `hmf init` writes apply everywhere, not just inside
   registered repos.

   **All of this is an opencode plugin.** A project whose `mouse.yaml` selects
   `provider: claude` is spawned as `claude -p` and loads no plugin, so it gets
   no command rules, no fs rules, and no cross-project edit protection.

4. Open opencode in a registered repo. The agent now has 6 tools:
   `post_message`, `task_status`, `report_progress`, `read_channel`,
   `read_thread`, `list_project_agents`. The `from` field is auto-detected
   from cwd.

## Config (per repo)

- `MOUSE.md` — agent runbook/guide
- `mouse.yaml` — agent binary, model, permissions, a2a
- `AGENTS.md` — OpenCode instruction file (references MOUSE.md)

See `examples/`.

## Watching delegated work

Children run as separate processes, so they can't report into the session that
dispatched them. Two views, depending on where you are:

**From another terminal — live dashboard:**

```bash
hmf monitor            # every task, running ones first
hmf monitor --active   # only what's still running
```

```
 hmf monitor  ● 1 working · ✗ 1 failed · 4 tasks                                           21:40:34 
╭────────────────────────────────────╮╭────────────────────────────────────────────────────────────╮
│ ▎ ● #214  frontend +1        7m00s ││ #214  backend → frontend +1                      ● working │
│ ▎ ▰▱▱▱▱ 1/4   ✗1 ship the login f… ││ ▰▰▰▱▱▱▱▱▱▱▱▱  1/4 done · 7m00s                             │
│   ✓ #213  penny-pincher      1m00s ││ ────────────────────────────────────────────────────────── │
│   ▰▰▰▰▰ 4/4   bump dependencies    ││ task                                                       │
│   ✗ #212  store              1m00s ││   ship the login flow end to end                           │
│   ▰▱▱▱▱ 1/5   migrate the session… ││                                                            │
│   ✓ #211  his-mouse-friday   9m00s ││ sessions · 2                                               │
│   ····· —     fix the flaky daemo… ││   backend  (dispatcher)                                    │
│                                    ││     └ 1. ● working  frontend  6m00s  ses_fe01  pid 771     │
│                                    ││       └ 2. ✗ failed   store  1m00s  ses_st03               │
│                                    ││                                                            │
│                                    ││   open one:  cd /tmp/fe && opencode -s <session id>        │
│                                    ││                                                            │
│                                    ││ work items · 1/4 done                                      │
│                                    ││   ✓ define the auth contract                               │
│                                    ││   ▸ build the form                                         │
│                                    ││   ○ wire the gateway route                                 │
│                                    ││   ○ migrate sessions table                                 │
╰────────────────────────────────────╯╰────────────────────────────────────────────────────────────╯
 ↑↓ move · tab focus · d del item · a active only · r refresh · q quit
```

On a terminal at least 96 columns wide the list and the selected task sit side
by side, so moving the cursor updates the detail immediately — no opening and
closing to compare two tasks. Narrower terminals fall back to a plain list,
with `enter` opening a task and `esc` going back.

Each list entry is two lines: status mark, thread id and the project doing the
work on the first, work-item progress and the instruction on the second. `●`
is running, `✓` finished, `✗` failed, and the elapsed time on the right is
time actually spent working. A task that pulled in more than one project shows
`frontend +1`; who *dispatched* it is in the detail header, which reads
`backend → frontend +1`, or `you` when the task came from a human in an
unregistered directory.

A conversation that was retried, resumed, or handed between projects keeps all
of that on one entry, and running work sorts to the top so it is never buried
under history.

The id is the thread id, so anything you spot is directly actionable:
`hmf watch 214`, or `task_status(message_id=214)` from an agent.

| key | |
|---|---|
| `↑` `↓` / `j` `k` | move (list scrolls) |
| `g` / `G` | jump to first / last |
| `tab` | switch focus between list and detail (wide terminals) |
| `enter` | focus the detail pane (or open a task on narrow terminals) |
| `esc` | back to the list |
| `a` | toggle all / running-only |
| `d` | in a task: pick a work item, `d` again to delete it (`y` confirms) |
| `r` | refresh now |
| `q` | quit |

The detail pane is a read-only view of the whole exchange — who asked, what
was asked, each attempt, the work items, and the **conversation itself**:
dispatches indented left, replies indented right, so the back-and-forth
between parent and child reads at a glance. Its header pins the identity,
status and progress in place while the body scrolls under them.

```
#165  his-mouse-friday → mouse-for-sale                              ✓ done
▰▰▰▰▰▰▰▰▰▰▰▰  1/1 done · 1m11s
──────────────────────────────────────────────────────────────────────────

task
  Read-only, NO edits. todo_add 'count routes' then count dirs under
  src/routes, mark done. Reply done with the number.

work items · 1/1 done
  ✓ count routes

conversation
  21:40 his-mouse-friday → haydn/mouse-for-sale
    Read-only, NO edits. todo_add 'count routes' then count dirs under
    src/routes, mark done. Reply done with the number.

      21:41 mouse-for-sale ↩ done
        Done. 4 route dirs under src/routes: login, pockets, settings,
        transactions. Read-only, no edits.
```

A parent that sends follow-ups mid-task shows them interleaved, so a long
multi-turn exchange reads in order.

Every task shows who ran it. The dispatcher sits at the top and each spawned
session hangs under whoever engaged it, so a hand-off (one agent pulling in a
second project) reads as a chain rather than unrelated attempts:

```
sessions · 3
  his-mouse-friday  (dispatcher)
    ├ 1. ✓ done     penny-pincher   5m00s  ses_7f3a2b9c1d4e5f6a  ·3 runs, 1 failed
      └ 2. ● working  mouse-for-sale  1m01s  ses_9b8c7d6e5f4a3b2c  pid 42
    └ 3. ✗ failed   ledger          0m12s  ses_1a2b3c4d5e6f7a8b

  open one:  cd /path/to/project && opencode -s <session id>
```

Each agent hangs under whoever engaged it, so a task that fans out to several
projects reads as a tree: the two agents the dispatcher engaged sit at the
first level, and the one `penny-pincher` pulled in nests under it. The list
entry carries the same fan-out in miniature — `frontend +3` names the first
worker and counts the rest, and a `✗2` beside the progress bar flags children
that failed while the task as a whole is still running.

One line per child agent, not per spawn. Resuming a task reuses its opencode
session id, so a task picked up three times leaves three session rows carrying
a single conversation — they collapse into one entry with `·3 runs`, and the
time shown is the sum of the runs, excluding the gaps between them. A failed
run among them is called out rather than hidden by the collapse.

Those are opencode session ids, so any agent listed can be reopened
directly — `cd` into that project and pass the id to `opencode -s`. opencode
resumes per-directory, which is why the hint carries the path as well as the
id. A spawn whose session id was never captured is never merged with another
on a guess; it stays its own entry, listed by its hmf name.

Work items can be deleted from here — press `d` to pick one, `d` again to
delete, `y` to confirm. An agent that loses track of a work item can strand it
as permanently pending, and this is where you notice. From the shell:
`hmf task show <thread_id>` to see ids, `hmf task delete <id>`.

The elapsed time is how long a task *ran* — it freezes when the task ends rather
than counting up forever, and on a task with several attempts it sums the
attempts rather than spanning the first start to now. A task you follow up on
hours later reports the work, not the wait. A finished task whose duration was
never recorded shows `?`; a mix of known and unknown attempts shows `32m+?`.

Piping works too — `hmf monitor | tee log.txt` prints one plain snapshot
instead of starting the interactive view.

**Knowing it picked up at all.**

The moment a child process spawns, the daemon posts a pickup notice on the
thread — before the agent has said anything:

```
      21:40:23 haydn/mouse-for-sale ↩ [hmf]
        working on it — agent spawned as 29898-mouse-for-sale (pid 15796)
```

It comes from the daemon, not the agent, and that is the point: an agent asked
to "reply that you started" cannot report the failure where it never started.
The ack separates *never spawned* from *working quietly* — the two cases that
otherwise look identical from the parent. It lands in `read_thread` and in
`task_status`'s `last_update` immediately, is tagged `hmf` in the monitor so
it is never mistaken for the child's own words, and is kept out of
`read_channel` since the lobby is not its audience.

It carries status `ack`, so it never counts as completion.

**What the child says about itself.**

A spawned agent is told to call `report_progress` once it realises the job
runs longer than a couple of minutes, and again whenever its estimate moves:

```
      21:43:10 haydn/mouse-for-sale ↩ [progress]
        decoding the screenshot to locate the flash element (eta ~12min)
```

`task_status` surfaces it as three fields:

```
progress_note:     decoding the screenshot to locate the flash element
eta_minutes:       12
progress_age_secs: 95
```

Read the ETA against the age. The note and the estimate are the child's
claims; the age is fact. Together they separate "on track" from "said ten
minutes, then went quiet forty minutes ago" — an estimate alone gets more
misleading the staler it is.

Reporting costs the parent nothing: the message posts with status `progress`
and never wakes anyone. Agents that reach for the shell first can use
`hmf progress "<note>" --eta 12` instead.

**From inside the dispatching session — `task_status`:**

That MCP tool returns the same detail (project, elapsed, `todos_done/total`,
`current_step`, the full todo list, and the child's latest reply line). It's
the only option that works without a second terminal, but it reports when
called rather than continuously.

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

## Cleaning up history

Every dispatch leaves messages, a session row, and todos behind, so `hmf
monitor` fills up with old runs. Clear it:

```bash
hmf prune                      # everything (asks for confirmation)
hmf prune --older-than 168h    # keep the last week
hmf prune --yes                # skip the prompt
```

Workspaces and projects are **never** touched — your registered repos stay
registered. Threads with a session still `active` are skipped too, so pruning
mid-task can't orphan a running agent. The file is vacuumed afterwards, so the
space is actually reclaimed rather than just marked free.

Deletion is irreversible; `cp ~/.hmf/hmf.db ~/.hmf/hmf.db.bak` first if you
want the history back.

## Logs

Everything goes to one file, `~/.hmf/hmf.log` — daemon events, agent
stdout/stderr, and (when backgrounded) the daemon's own stderr:

```bash
tail -f ~/.hmf/hmf.log
```

Each line is `<timestamp> [<event>] key=value...`. Events are `rpc`, `post`,
`wake`, `spawn`, `capture`, `exit`, `reap`, and `agent#<session> <name>` for the
child process's own output.

Three greps cover most debugging:

```bash
grep ERROR ~/.hmf/hmf.log      # every failure, one line each
grep thread=68 ~/.hmf/hmf.log  # one task end to end, across all its agents
grep 'agent#' ~/.hmf/hmf.log   # what the spawned agents printed
```

`thread=<id>` is on every post, wake, spawn and exit line, and it's the same id
`task_status`/`read_thread` take — so a task id from the monitor replays the
whole task, including a second project engaged on the same thread.

Agent output is the useful part: without it a runtime error (bad model, dead
`-s` session id, auth prompt) is invisible and just looks like an agent that
never replied.

Read-only polls (`read_thread`, `todo_list`, `task_status`, ...) log a one-line
summary rather than full bodies — an open monitor TUI would otherwise push the
real events out of the file.

The file is capped at 40MB. At the cap it rotates to `~/.hmf/hmf.log.1`,
dropping the previous backup — at most two files. Start a background daemon
with `nohup hmf up >> ~/.hmf/hmf.log 2>&1 &` (what `scripts/install.sh` does)
so panics land there too; in a terminal, `hmf up` also mirrors to stderr.

## Troubleshooting

**One project replies, a second one on the same thread never does.**
Check the log for the second project's spawn line:

```bash
grep '\[spawn\]' ~/.hmf/hmf.log
```

If its argv carries `run -s <id>` and that `<id>` also appears on another
project's session in `hmf session list`, it resumed a session that only exists
in the *other* project's directory, so it hangs forever without replying. Fixed
— the resume lookup is scoped to `project_id`, not just the thread. An agent
already stuck this way stays `active` until you `kill <pid>` it (pid is in
`hmf session list`).

**`delete failed: thread N still has 1 running agent(s)`.**
The error now names the session and pid. A dead pid is reaped automatically
(delete and prune both run `ReapDeadSessions` first), so this only appears when
a process really is alive. Either wait for it, or `kill <pid>` and delete
again — a hung agent that will never reply is safe to kill.

**`MCP error -32001: Request timed out` on every `task_status` call.**
The `hmf` MCP server has no `timeout` set, so opencode is using its 5-second
default while `task_status` blocks for up to 5 minutes. Add
`"timeout": 330000` to the `hmf` entry in your `opencode.json` (see *Wire the
MCP server into opencode*) and restart the session — MCP config is read at
connection time.

**Spawned agents do nothing, or refuse multi-file work.**
Check what agent they run as. hmf passes `--agent hmf-worker`; if that agent
isn't installed (`~/.config/opencode/agents/hmf-worker.md`), opencode falls
back to your `default_agent`, which may be a narrow one that refuses 3+ file
tasks or has no shell. Re-run the installer, or copy
`examples/agents/hmf-worker.md` into place.

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
  One cause is fixed: a second project on the same thread used to resume the
  first project's opencode session id and hang. Use `~/.hmf/hmf.log` to tell
  a real prompt problem from a spawn problem.

- [x] **Thread `to` field through `post_message` auto-fill.** Fixed in
  `internal/daemon/daemon.go:handlePost` — a reply that omits `to` now
  resolves it from the thread root (the other party relative to `from`),
  skipped for `status=done` to avoid waking the originator back on a
  worker's completion notice.

- [ ] **Session resume.** User should be able to reopen any spawned agent's
  session (`hmf session attach <id>`) and continue directly.

- [ ] **Real multi-agent scenario.** 3+ agents coordinating (payment →
  user-service → frontend) to prove the full team model.
