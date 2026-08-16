---
description: Guided setup for his-mouse-friday — register workspace + project, write config files
---
Interactive setup wizard for his-mouse-friday. Present numbered options, let the user pick. Do NOT scan the repo, read manifests, or infer anything. Use templates. One question at a time, wait for the answer, then proceed.

## Step 1: Check daemon

Run `hmf status`. 

- If it fails: respond "Daemon not running. Run `hmf up` in a separate terminal, then run /hmf-setup again." and STOP. Do nothing else.
- If running: show the status line (workspaces/projects/sessions count), then continue to Step 2.

## Step 2: Workspace

Show the user a numbered list of existing workspaces by running:

```bash
hmf status
```

Then ask ONE question:

```
Workspace:
1. <existing-ws-1>
2. <existing-ws-2>
3. Create new workspace
Pick a number: 
```

- If they pick an existing workspace → use that name, go to Step 3.
- If they pick "Create new" → ask "New workspace name: " then run `hmf workspace add <name>`. Go to Step 3.

Do not proceed until the user answers.

## Step 3: Project name + path

Suggest a project name from the current directory:

```bash
basename "$(pwd)"
```

Ask ONE question:

```
Project name (enter to accept "<suggested>"): 
```

- If user presses enter → use the suggested name.
- If user types a name → use that.

Path is always the current working directory. Do not ask for path.

Then run:

```bash
hmf project add <name> "$(pwd)" --workspace <ws>
```

- If it errors: show the error, ask for a different name, retry.
- If success: continue to Step 4.

## Step 4: Config files

Check which files exist in the repo root (`ls mouse.yaml MOUSE.md AGENTS.md 2>/dev/null`). For each MISSING file, tell the user what you'll write and ask yes/no:

```
Write mouse.yaml? (Y/n): 
```

If yes (or user presses enter), write the TEMPLATE below. If no, skip. If file already exists, skip silently.

### mouse.yaml template (do not customize — user edits later)

```yaml
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

### MOUSE.md template (do not customize — user edits later)

```markdown
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

Replace `<project-name>` with the actual project name. Leave the other placeholders as-is — the user fills them in.

### AGENTS.md template

```markdown
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

Stop. Do not do anything else.
