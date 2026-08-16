# his-mouse-friday Agent Runbook

## Ownership
Per-directory AI agent orchestration harness. Root orchestrator spawns
scoped sub-agents; each directory declares its own config and rules. Agents
own their repo and coordinate across repos via a shared comms layer instead
of editing foreign code directly.

## Conventions
- Go 1.26.5, module `github.com/herdanis/his-mouse-friday`
- CLI: cobra. MCP: modelcontextprotocol/go-sdk. Storage: modernc.org/sqlite.
- Install: `go install ./cmd/hmf` and `go install ./cmd/hmf-mcp`
- No test/lint command defined yet (greenfield).
- Commit style: Conventional Commits.

## Dependencies
- No external service dependencies detected. Agents communicate via the hmf
  daemon's MCP tools, not over the network.

## Guardrails
- No commands denied (agent may run any command).
- No commands require approval.
- No files denied (agent may read/write any file).
- A2A policy: inbound=true, outbound=true.
- Rule: do NOT modify other services directly — engage their agent via hmf.
- Rule: if unsure, ask the user before making changes outside src/.

## Escalation
- If the task requires changes in another registered project, use
  `engage_project_agent` to delegate to that project's agent.
- If the task is unclear or crosses ownership boundaries, ask the user.
- If a denied command is needed, ask the user to run it manually.
