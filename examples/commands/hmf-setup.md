---
description: Guided setup for his-mouse-friday — register workspace + project, write config files
---
Interactive setup wizard for his-mouse-friday. Ask one question at a time, wait for the answer, proceed.

HARD RULES — violation = failure:
- Do NOT show file contents in the chat. Ever. Write files silently.
- Do NOT interpret user input. If they type "yes", that IS the project name.
- Do NOT run --help on any command. Do NOT scan the repo. Do NOT read manifests.
- Run exact commands below. Parse their output. Show numbered options.

## Step 1: Check daemon

Run: `hmf status`

- If it fails: respond "Daemon not running. Run `hmf up` in a separate terminal, then run /hmf-setup again." and STOP.
- If running: say "Daemon running." and continue.

## Step 2: Check hmf plugin

The hmf plugin (`plugins/hmf/plugin.ts`) is what actually enforces edit/bash
protection and mouse.yaml's deny/ask lists at the code level. Without it,
registering a project in Step 3 only *claims* to be guarded — nothing
blocks anything. Check both that it's installed and that it's wired in.

Run:

```
OPENCODE_CONFIG="${OPENCODE_CONFIG:-$HOME/.config/opencode}"
test -f "$OPENCODE_CONFIG/plugins/hmf/plugin.ts" && echo "FILE: ok" || echo "FILE: missing"
CFG="$OPENCODE_CONFIG/opencode.jsonc"; [ -f "$CFG" ] || CFG="$OPENCODE_CONFIG/opencode.json"
echo "CFG: $CFG"
grep -Fq "plugins/hmf/plugin.ts" "$CFG" 2>/dev/null && echo "WIRED: ok" || echo "WIRED: missing"
```

- `FILE: missing` → respond "hmf plugin not installed. Run `./scripts/install.sh`
  from the his-mouse-friday repo (or re-run it if already installed once),
  then run /hmf-setup again." and STOP.
- `FILE: ok` and `WIRED: missing` → respond exactly:
  ```
  hmf plugin installed but not wired in. Add "./plugins/hmf/plugin.ts" to the
  plugin[] array in <CFG path from above>, then run /hmf-setup again.
  ```
  and STOP. Do NOT edit `<CFG>` yourself — it may be JSONC (comments/trailing
  commas) and a naive rewrite can corrupt it.
- `FILE: ok` and `WIRED: ok` → say "hmf plugin active." and continue.

## Step 3: Workspace

Run: `hmf workspace list`

Parse output (one workspace name per line). Show the user:

```
Workspace:
1. <name-1>
2. <name-2>
3. Create new workspace
Pick a number:
```

- Number → use that workspace name.
- Create new → ask "New workspace name:", then run `hmf workspace add <name>`.

Wait for the user's answer.

## Step 4: Project name

Run: `basename "$(pwd)"`

Show the user:

```
Project name:
1. <suggested>
2. Type a different name
Pick a number:
```

- 1 → use the suggested name.
- 2 → ask "Project name:", then use what they type.
- Any other reply → ask again.

Then run: `hmf project add <name> "$(pwd)" --workspace <ws>`

- Error → show it, ask for a different name.
- Success → continue. The project is now auto-guarded (direct edits/commands
  from outside blocked by the hmf plugin, `plugins/hmf/plugin.ts`, reading
  the registry) — provided that plugin is installed and wired into
  `~/.config/opencode/opencode.jsonc`'s `plugin[]` array (`install.sh` does
  this; not verified by this wizard). No need to run 'hmf guard'.

## Step 5: Permissions

Before writing config files, ask the user what this project's agent should NOT do. Three questions. Each question: user types patterns (comma-separated), enters for defaults, or types "allow" to allow all (no restrictions).

### Question 1: Command restrictions (deny)

Show the user:

```
Commands to DENY (agent cannot run these):
Type patterns separated by commas, or:
  - press enter for defaults: kubectl delete, kubectl apply, gcloud * delete, aws * delete,
    rm -rf /, rm -rf ~, git push --force, git push -f, git reset --hard, git clean -fd,
    git clean -xfd, sudo, chmod -R 777, dd if=, mkfs, shutdown, reboot
  - type "allow" to deny nothing (allow all commands)
```

- Patterns → use them as the deny list.
- Enter (empty) → use defaults shown.
- "allow" → empty deny list (no commands blocked).

### Question 2: Command approval (ask)

Show the user:

```
Commands to ASK before running (agent needs approval):
Type patterns separated by commas, or:
  - press enter for defaults: kubectl scale, rm -rf, git push, npm publish, docker system prune
  - type "allow" to ask for nothing (no approval needed)
```

- Patterns → use them as the ask list.
- Enter (empty) → use default shown.
- "allow" → empty ask list (no approval prompts).

### Question 3: File restrictions (deny)

Show the user:

```
Files to DENY access (agent cannot read/write these):
Type patterns separated by commas, or:
  - press enter for defaults: .env, *.key, .terraform/**, secrets/**
  - type "allow" to deny nothing (allow all files)
```

- Patterns → use them as the deny list.
- Enter (empty) → use defaults shown.
- "allow" → empty deny list (no files blocked).

Store the three answers. Use them when writing mouse.yaml + opencode.json in Step 6.

## Step 6: Write config files

Run: `ls mouse.yaml MOUSE.md AGENTS.md opencode.json 2>/dev/null`

Write all missing files immediately. Do NOT ask which ones. Do NOT show file contents. Do NOT print what was written. Just write silently.

For each file that ALREADY exists, skip it.

### If mouse.yaml is missing, write it using the user's answers from Step 5.

Use this structure, filling in the deny/ask lists from what the user answered:

```
agent:
  primary:
    provider: opencode
    model: modelnyaw/glm-5.2
  # secondary:              # fallback when primary unavailable
  #   provider: claude
  #   model: default
permissions:
  fs:
    deny:
      - "<file-deny-pattern-1>"
      - "<file-deny-pattern-2>"
  commands:
    deny:
      - "<cmd-deny-pattern-1>"
      - "<cmd-deny-pattern-2>"
    ask:
      - "<cmd-ask-pattern-1>"
a2a:
  allow_inbound: true
  allow_outbound: true
```

If a list is empty (user said "none"), omit that key entirely (e.g. no `ask:` block if empty).

### If MOUSE.md is missing, generate it with REAL content — no placeholders.

Do NOT write angle-bracket placeholders like `<what this agent owns>`. Generate actual content by combining THREE sources:

**Source 1 — User input (ask first):**

Before reading any files, ask the user:

```
Additional details for this project's agent runbook? (optional)
You can provide:
- What this project owns / does
- Coding conventions, test commands, commit style
- Dependencies on other services
- Anything else the agent should know
Type your details, or press enter to skip (AI will infer from repo + memories):
```

- If user types details → use them as the primary source, supplement with repo docs + memories.
- If user presses enter (empty) → rely on repo docs + memories only.

**Source 2 — Existing docs (read if present, do NOT scan whole repo):**

Read these files if they exist:
- `README.md` or `README*`
- `AGENTS.md` or `CLAUDE.md`
- `Makefile` or `package.json` or `go.mod` or `Cargo.toml` or `pyproject.toml`
- `.github/workflows/*` (one or two files, not all)

**Source 3 — AI memories (if available):**

Check AI memories (use mem_search if available) for this project — past decisions, conventions, architecture, known issues.

**Merging:**

- User input takes priority — if the user said "test with pytest", that wins over a Makefile that says `make test`.
- Repo docs fill gaps the user didn't cover.
- Memories supplement both (past decisions, gotchas learned in prior sessions).
- All three combine into one coherent runbook. No source attribution needed in the output.

Write MOUSE.md with this structure, filled with REAL merged content:

```markdown
# <project-name> Agent Runbook

## Ownership
<What this service/project does, its main responsibility.
From user input first, then README/package name, then memories.
If still unclear, write a best guess based on project name + "(review needed)".>

## Conventions
<Language + framework (from manifests or user input)
- Test command (user input > Makefile/scripts > language default)
- Lint/format command if found or user-provided
- Commit style (user input > git log > "Conventional Commits")
- Any conventions from AGENTS.md/CLAUDE.md/memories>

## Dependencies
<Other services this project calls (user input > imports/config > docker-compose)
- Services that call this one (if mentioned in docs/user input/memories)
- If none found, write "No external service dependencies detected.">

## Guardrails
<Commands the agent CANNOT run (from Step 5 deny list)
- Commands requiring approval (from Step 5 ask list)
- Files the agent CANNOT access (from Step 5 file deny list)
- A2A policy: inbound=<allow_inbound>, outbound=<allow_outbound>
- Rule: do NOT modify other services directly — engage their agent via hmf
- Rule: if unsure, ask the user before making changes outside src/
- Any additional restrictions from user input or memories>

## Escalation
<If the task requires changes in another registered project, use
  engage_project_agent tool to delegate to that project's agent.
- If the task is unclear or crosses ownership boundaries, ask the user.
- If a denied command is needed, ask the user to run it manually.
- Known issues or gotchas from memories (if any).>
```

Rules:
- Every section must have real content. No `<placeholder>` text.
- User input always takes priority over inferred content.
- If you cannot infer something and the user didn't provide it, write a best guess and append " (review needed)".
- Keep it concise — this is a runbook, not documentation. Under 60 lines total.
- Do NOT print the contents to the chat. Write silently.

### If AGENTS.md is missing, write this exact content:

```
See ./MOUSE.md for this project's agent guide and ownership scope.
```

### Generate opencode.json (mouse.yaml is the real gate)

If `opencode.json` already exists in the repo root, skip this step (don't overwrite user's config). Note it was skipped.

If it does NOT exist, write `opencode.json` with this exact structure:

```json
{
  "$schema": "https://opencode.ai/config.json",
  "permission": {
    "bash": {
      "*": "ask"
    },
    "edit": "allow"
  }
}
```

Do NOT put the Step 5 deny/ask patterns here as `"deny"`/`"ask"` values, and do
NOT set `bash` to a blanket `"allow"`. Two separate mechanisms already cover it:

- **Interactive sessions** have a TTY, so `"ask"` is answerable and correct.
- **hmf-spawned agents** run headless, where an unanswerable `ask` would hang —
  so the daemon spawns them with `--auto`, which auto-approves only what is not
  explicitly denied.
- **The real guardrail** is the hmf plugin (`plugins/hmf/plugin.ts`) reading
  `mouse.yaml`'s `commands.deny`/`ask`. It throws a clean error back to the
  agent rather than hanging, and it still fires under `--auto` (verified).

A blanket `bash: "*": "allow"` would defeat all of this and was flagged by a
security review — don't reintroduce it.

After writing, report ONLY this line (listing only files you wrote):

```
Wrote: mouse.yaml, MOUSE.md, AGENTS.md, opencode.json
```

## Step 7: Done

Print exactly:

```
Registered: <workspace>/<project>

Next:
- Open opencode in this repo — agent now has hmf tools
- Edit mouse.yaml + MOUSE.md to fit your project
- Daemon must be running (hmf up) for orchestration
```

Stop. Nothing else.
