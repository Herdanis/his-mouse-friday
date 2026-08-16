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

## Step 2: Workspace

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

## Step 3: Project name

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
- Success → continue.

## Step 4: Permissions

Before writing config files, ask the user what this project's agent should NOT do. Three questions. Each question: user types patterns (comma-separated), enters for defaults, or types "allow" to allow all (no restrictions).

### Question 1: Command restrictions (deny)

Show the user:

```
Commands to DENY (agent cannot run these):
Type patterns separated by commas, or:
  - press enter for defaults: kubectl delete, kubectl apply, gcloud * delete, aws * delete
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
  - press enter for default: kubectl scale
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

Store the three answers. Use them when writing mouse.yaml + opencode.json in Step 5.

## Step 5: Write config files

Run: `ls mouse.yaml MOUSE.md AGENTS.md opencode.json 2>/dev/null`

Write all missing files immediately. Do NOT ask which ones. Do NOT show file contents. Do NOT print what was written. Just write silently.

For each file that ALREADY exists, skip it.

### If mouse.yaml is missing, write it using the user's answers from Step 4.

Use this structure, filling in the deny/ask lists from what the user answered:

```
agent:
  primary:
    provider: opencode
    model: default
  secondary:
    provider: ""
    model: ""
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

### If MOUSE.md is missing, write this exact content (replace <project-name> with the actual project name):

```
# <project-name> Agent Runbook

## Ownership
<what this agent owns>

## Conventions
<coding standards, commit style, test commands>

## Dependencies
<other services this one calls or is called by>

## Boundaries
<what this agent must NOT do, escalation paths>
```

### If AGENTS.md is missing, write this exact content:

```
See ./MOUSE.md for this project's agent guide and ownership scope.
```

### Generate opencode.json (command enforcement)

Use the command deny/ask lists from Step 4 (the same answers used for mouse.yaml). Do NOT re-read mouse.yaml — use the stored answers.

If `opencode.json` already exists in the repo root, skip this step (don't overwrite user's config). Note it was skipped.

If it does NOT exist, write `opencode.json` with this structure (use the actual patterns from Step 4):

```json
{
  "$schema": "https://opencode.ai/config.json",
  "permission": {
    "bash": {
      "<deny-pattern-1>": "deny",
      "<deny-pattern-2>": "deny",
      "<ask-pattern-1>": "ask",
      "*": "ask"
    }
  }
}
```

Rules:
- Every pattern from `commands.deny` → value `"deny"`.
- Every pattern from `commands.ask` → value `"ask"`.
- Add `"*": "ask"` as the last entry (default: ask for anything not explicitly allowed).
- If both deny and ask lists are empty, still write `"*": "ask"` so the agent asks before running unknown commands.

After writing, report ONLY this line (listing only files you wrote):

```
Wrote: mouse.yaml, MOUSE.md, AGENTS.md, opencode.json
```

## Step 6: Done

Print exactly:

```
Registered: <workspace>/<project>

Next:
- Open opencode in this repo — agent now has hmf tools
- Edit mouse.yaml + MOUSE.md to fit your project
- Daemon must be running (hmf up) for orchestration
```

Stop. Nothing else.
