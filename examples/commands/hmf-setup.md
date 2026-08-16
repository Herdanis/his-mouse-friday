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
Project name (press enter for "<suggested>", or type a name):
```

- If the user's reply is empty → use the suggested name.
- If the user's reply is non-empty → use it AS THE PROJECT NAME. Do not interpret it as yes/no/confirmation.

Then run: `hmf project add <name> "$(pwd)" --workspace <ws>`

- Error → show it, ask for a different name.
- Success → continue.

## Step 4: Write config files

Run: `ls mouse.yaml MOUSE.md AGENTS.md 2>/dev/null`

For each file that does NOT exist, write it using the write tool. Do NOT print the contents to the chat. Do NOT show what was written. Just write silently.

For each file that ALREADY exists, skip it.

### If mouse.yaml is missing, write this exact content:

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
      - ".env"
      - "*.key"
a2a:
  allow_inbound: true
  allow_outbound: true
```

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

After writing, report ONLY this line (listing only files you wrote):

```
Wrote: mouse.yaml, MOUSE.md, AGENTS.md
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
