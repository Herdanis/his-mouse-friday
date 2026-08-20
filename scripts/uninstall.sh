#!/usr/bin/env bash
set -euo pipefail

# ============================================
# his-mouse-friday uninstaller
# ============================================
# Removes the opencode plugin, slash commands, and the
# hmf / hmf-mcp binaries. Leaves opencode.json untouched.

OPENCODE_CONFIG="${OPENCODE_CONFIG:-$HOME/.config/opencode}"

info() { printf '\033[36m▸\033[0m %s\n' "$1"; }
ok()   { printf '\033[32m✓\033[0m %s\n' "$1"; }

removed=0

# Plugin + slash commands.
for rel in \
  "plugins/hmf.ts" \
  "commands/hmf-setup.md" \
  "commands/hmf-register.md"; do
  p="$OPENCODE_CONFIG/$rel"
  if [ -f "$p" ]; then
    rm -f "$p"
    ok "removed $p"
    removed=$((removed + 1))
  fi
done

# Binaries from GOPATH/bin (or GOBIN).
gobin="$(go env GOBIN 2>/dev/null || true)"
[ -n "$gobin" ] || gobin="$(go env GOPATH 2>/dev/null)/bin"
for bin in hmf hmf-mcp; do
  p="$gobin/$bin"
  if [ -f "$p" ]; then
    rm -f "$p"
    ok "removed $p"
    removed=$((removed + 1))
  fi
done

if [ "$removed" -eq 0 ]; then
  info "nothing to remove"
fi

cat <<EOF

Manual step: remove the "hmf" block from
$OPENCODE_CONFIG/opencode.json

EOF
ok "done"
