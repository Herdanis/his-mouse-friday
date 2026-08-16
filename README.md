# his-mouse-friday

Per-directory AI agent orchestration harness. Each repo gets a dedicated AI
agent "engineer" that owns it. When work crosses a dependency boundary, an
agent engages the other repo's agent via a shared comms layer instead of
editing foreign code directly.

## Install (dev)

    go build ./cmd/hmf ./cmd/hmf-mcp

## Usage

    hmf up                                      # start daemon
    hmf workspace add companyA
    hmf project add payment-service --workspace companyA --path ~/code/payment
    hmf status

Open opencode in a registered repo; the agent gets `hmf-mcp` tools to engage
other project agents.

## Config (per repo)

- `MOUSE.md` — agent runbook/guide
- `mouse.yaml` — agent binary, model, permissions, a2a
- `AGENTS.md` — OpenCode instruction file (references MOUSE.md)

See `examples/`.
