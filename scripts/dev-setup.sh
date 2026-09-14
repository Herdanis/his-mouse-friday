#!/usr/bin/env bash
# ============================================
# hmf dev setup — local machine, for testing
# ============================================
# Installs the single hmf binary, (re)starts the daemon (this triggers the
# one-time schema migration), registers the current repo, and wires the
# `hmf mcp` shim into ~/.config/opencode/opencode.jsonc.
# Idempotent: safe to re-run.
set -euo pipefail

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GLOBAL_CFG="${OPENCODE_CONFIG:-$HOME/.config/opencode}/opencode.jsonc"

ok()   { printf '\033[32m✓\033[0m %s\n' "$1"; }
info() { printf '\033[36m▸\033[0m %s\n' "$1"; }

# 1. Install the binary from this checkout.
info "installing hmf from $REPO_DIR"
( cd "$REPO_DIR" && go install ./cmd/hmf )
ok "hmf → $(go env GOPATH)/bin/hmf"

# 2. Restart the daemon so the new binary owns it (old binary + old schema
#    keep serving until killed; migration runs on the new daemon's first open).
#    NOTE: when run from an interactive shell, `nohup hmf up &` survives;
#    some CI/tool shells reap background children — check `hmf status` after.
if hmf status >/dev/null 2>&1; then
  info "daemon running — restarting with new binary"
  hmf down || true
fi
nohup hmf up >/dev/null 2>&1 &
for i in 1 2 3 4 5 6 7 8 9 10; do
  sleep 1
  hmf status >/dev/null 2>&1 && break
done
hmf status >/dev/null 2>&1 || { echo "daemon failed to start — check ~/.hmf/hmf.log"; exit 1; }
ok "daemon up (schema migrated if this was an old DB)"

# 3. Register this repo (bare name) if not already.
PROJ_NAME="$(basename "$REPO_DIR")"
if hmf project list 2>/dev/null | grep -q "$PROJ_NAME"; then
  ok "project '$PROJ_NAME' already registered"
else
  hmf project add "$PROJ_NAME" "$REPO_DIR" 2>/dev/null || true
  hmf project list 2>/dev/null | grep -q "$PROJ_NAME" ||
    { echo "project '$PROJ_NAME' registration failed"; exit 1; }
  ok "project '$PROJ_NAME' registered → $REPO_DIR"
fi

# 3b. Sync the opencode plugin (permissions enforcement, own-repo rule).
PLUGIN_SRC="$REPO_DIR/examples/plugins/hmf/plugin.ts"
PLUGIN_DST="${OPENCODE_CONFIG:-$HOME/.config/opencode}/plugins/hmf/plugin.ts"
if ! diff -q "$PLUGIN_SRC" "$PLUGIN_DST" >/dev/null 2>&1; then
  mkdir -p "$(dirname "$PLUGIN_DST")"
  cp "$PLUGIN_SRC" "$PLUGIN_DST"
  ok "plugin synced → $PLUGIN_DST"
else
  ok "plugin already current"
fi

# 4. Wire the MCP shim into the global opencode config. The file is JSONC
#    (comments + trailing commas) so jq can't be used — insert an hmf block
#    after the "mcp": { line if one isn't present already.
if grep -q '"hmf"' "$GLOBAL_CFG" 2>/dev/null; then
  ok "mcp.hmf already present in $GLOBAL_CFG"
elif [ -f "$GLOBAL_CFG" ] && grep -q '"mcp"' "$GLOBAL_CFG"; then
  python3 - "$GLOBAL_CFG" <<'PY'
import sys, re
p = sys.argv[1]
s = open(p).read()
block = '''    "hmf": {
      "command": ["hmf", "mcp"],
      "enabled": true,
      "type": "local",
    },
'''
s = re.sub(r'("mcp"\s*:\s*\{)', r'\1\n' + block, s, count=1)
open(p, "w").write(s)
PY
  ok "mcp.hmf added to $GLOBAL_CFG"
else
  info "no mcp block in $GLOBAL_CFG — add manually:"
  printf '  "mcp": { "hmf": { "command": ["hmf", "mcp"], "enabled": true, "type": "local" } }\n'
fi

echo
ok "done. open opencode in $REPO_DIR and the hmf tools are live."
