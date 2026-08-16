# his-mouse-friday

Per-directory AI agent orchestration harness. Each repo gets a dedicated AI
agent "engineer" that owns it. When work crosses a dependency boundary, an
agent engages the other repo's agent via a shared comms layer instead of
editing foreign code directly.

## Install (dev)

    go install ./cmd/hmf
    go install ./cmd/hmf-mcp

## Setup

**Option A — Slash command (recommended):**

Copy the commands to your opencode config:

    cp examples/commands/hmf-*.md ~/.config/opencode/commands/

Then in any opencode session:
- `/hmf-setup` — full guided setup (workspace, project, config files, tailored MOUSE.md)
- `/hmf-register` — fast registration (one question, minimal config)

**Option B — Manual:**

1. Start the daemon:

       hmf up

2. Register a workspace + your repos:

       hmf workspace add companyA
       hmf project add payment-service ~/code/payment --workspace companyA
       hmf project add user-service ~/code/user-service --workspace companyA
       hmf status

3. Wire `hmf-mcp` into opencode. Either globally in `~/.config/opencode/opencode.json`:

       "mcp": {
         "hmf": {
           "type": "local",
           "command": ["hmf-mcp"],
           "enabled": true
         }
       }

   Or per-repo in `opencode.json` at the repo root (see `examples/opencode.json`).

4. Add `mouse.yaml` + `MOUSE.md` to each repo (see `examples/`).
   Set `a2a.allow_inbound: true` on repos other agents may engage.
   `mouse.yaml` declares command + filesystem permissions (gitignore-style:
   deny / ask / allow). `/hmf-setup` auto-generates `opencode.json` from it,
   so opencode enforces the policy natively — agents physically cannot run
   denied commands (e.g. `kubectl delete`, `gcloud * delete`).

5. Open opencode in a registered repo. The agent now has 5 tools:
   `engage_project_agent`, `post_message`, `read_channel`, `read_thread`,
   `list_project_agents`. The `from` field is auto-detected from cwd.

## Config (per repo)

- `MOUSE.md` — agent runbook/guide
- `mouse.yaml` — agent binary, model, permissions, a2a
- `AGENTS.md` — OpenCode instruction file (references MOUSE.md)

See `examples/`.
