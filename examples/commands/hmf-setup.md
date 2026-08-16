---
description: Guided setup for his-mouse-friday — register workspace + project, write config files
---
Interactive setup wizard for his-mouse-friday. Ask one question at a time, wait for the answer, proceed. Do NOT explore the CLI with --help, do NOT scan the repo, do NOT read manifests. Run exact commands below, parse output, show options.

## Step 1: Check daemon

Run: `hmf status`

- If it fails: respond "Daemon not running. Run `hmf up` in a separate terminal, then run /hmf-setup again." and STOP.
- If running: say "Daemon running." and continue.

## Step 2: Workspace

Run: `hmf workspace list`

Parse the output (one workspace name per line). Show the user:

```
Workspace:
1. <name-1>
2. <name-2>
3. Create new workspace
Pick a number:
```

- Number → use that workspace name.
- Create new → ask "New workspace name:", then run `hmf workspace add <name>`.

Wait for the user's answer before proceeding.

## Step 3: Project name

Run: `basename "$(pwd)"`

Show the user:

```
Project name (enter to accept "<suggested>"):
```

- Empty → use suggested.
- Typed → use that.

Then run: `hmf project add <name> "$(pwd)" --workspace <ws>`

- Error → show it, ask for a different name.
- Success → continue.

## Step 4: Config files

Run: `ls mouse.yaml MOUSE.md AGENTS.md 2>/dev/null`

For each file that does NOT exist, ask one yes/no question. Write the exact template if yes. Skip if no or if file exists.

### mouse.yaml

Ask: "Write mouse.yaml? (Y/n):"

If yes, write exactly:
```
agent:
  primary: opencode
  model: default
permissions:
  fs:
    paths:
      "src/**": allow
a2a:
  allow_inbound: true
  allow_outbound: true
```

### MOUSE.md

Ask: "Write MOUSE.md? (Y/n):"

If yes, write exactly (replace <project-name> with the actual name):
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

### AGENTS.md

Ask: "Write AGENTS.md? (Y/n):"

If yes, write exactly:
```
See ./MOUSE.md for this project's agent guide and ownership scope.
```

## Step 5: Done

Print exactly:

```
Registered: <workspace>/<project>

Next:
- Open opencode in this repo — agent now has hmf tools
- Edit mouse.yaml + MOUSE.md to fit your project
- Daemon must be running (hmf up) for orchestration
```

Stop. Nothing else.
