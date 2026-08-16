---
description: Quick-register current repo with hmf (workspace, project, minimal config)
---
Register the current repo with his-mouse-friday fast. Minimal interaction.

1. Check `hmf status`. If daemon down, tell user to run `hmf up` first, stop.
2. Ask for workspace name (one question). If it doesn't exist, create it with `hmf workspace add <name>`.
3. Suggest project name from cwd dirname. Ask: "Project name? (enter to accept <suggested>)"
4. Run `hmf project add <name> <cwd> --workspace <ws>`.
5. If `mouse.yaml` missing in repo root, write a minimal one:
   ```yaml
   agent:
     primary: opencode
     model: default
   a2a:
     allow_inbound: true
     allow_outbound: true
   ```
6. If `AGENTS.md` missing, write: `See ./MOUSE.md for this project's agent guide and ownership scope.`
7. Show: `Registered: <ws>/<name>`. Done.
