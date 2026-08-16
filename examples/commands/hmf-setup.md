---
description: Guided setup for his-mouse-friday — register workspace, project, generate config files
---
Guide the user through his-mouse-friday (hmf) setup interactively. Follow these steps in order. Use the available tools (bash, read, write, edit, glob) to execute. Show options clearly. Wait for user input at each decision point.

## Step 1: Check daemon

Run `hmf status`. If it fails (daemon not running), tell the user to run `hmf up` in a separate terminal first, then stop here. If running, show current state (workspaces, projects, sessions count) and continue.

## Step 2: Pick or create workspace

List existing workspaces by querying the daemon:

```bash
python3 -c "
import socket, json
s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
s.connect('/Users/herdanis/.hmf/daemon.sock')
req = {'method':'status','params':{},'id':1}
s.sendall((json.dumps(req)+'\n').encode())
buf=b''
while True:
    c=s.recv(4096)
    if not c: break
    buf+=c
    if b'\n' in buf: break
print(buf.decode().strip())
s.close()
"
```

Show the user:
- Existing workspaces (if any) as numbered options
- Option to create a new workspace

Ask: "Which workspace? (number or new name)"

If new: run `hmf workspace add <name>`. If existing: use the selected name.

## Step 3: Register the project

The current repo path is the cwd. Suggest a project name based on the directory name.

Ask: "Project name? (suggested: <dirname>)  Path? (default: <cwd>)"

Then run:

```bash
hmf project add <name> <path> --workspace <ws>
```

If it errors (duplicate, bad path), show the error and retry.

## Step 4: Generate config files

Check if `mouse.yaml`, `MOUSE.md`, `AGENTS.md` already exist in the repo root. For each missing file, generate it:

### mouse.yaml

Read the repo to infer: language (check for go.mod, package.json, Cargo.toml, etc.), framework, key dirs. Generate:

```yaml
agent:
  primary: opencode
  model: default
permissions:
  fs:
    paths:
      "<src-dir>/**": allow
a2a:
  allow_inbound: true
  allow_outbound: true
```

Adjust `permissions.fs.paths` based on the repo structure (e.g., deny `.env`, `.terraform/`, `*.key`).

### MOUSE.md

Generate a runbook tailored to the repo. Scan for: README, existing AGENTS.md, package manifests, main entrypoints, test commands. Structure:

```markdown
# <project> Agent Runbook

## Ownership
<inferred from README/package name>

## Conventions
<language, test cmd, lint cmd if found>

## Dependencies
<check for imports/requires of other services>

## Boundaries
Don't modify other services directly — engage their agent via hmf.
```

### AGENTS.md

```markdown
# AGENTS.md

See ./MOUSE.md for this project's agent guide and ownership scope.
```

Show the user each generated file and ask for confirmation before writing. If files already exist, skip (don't overwrite) and note it.

## Step 5: Verify

Run `hmf status` and show the result. Confirm the new project appears.

List the project's canonical ID: `<workspace>/<project>`.

## Step 6: Next steps

Tell the user:
- Open opencode in this repo — the agent now has hmf tools (engage_project_agent, post_message, read_channel, read_thread, list_project_agents)
- To engage another project's agent: the agent calls `engage_project_agent` with the target's `<workspace>/<project>` ID
- Daemon must be running (`hmf up`) for orchestration to work
- Config files (mouse.yaml) in other repos control inbound permission + agent binary
