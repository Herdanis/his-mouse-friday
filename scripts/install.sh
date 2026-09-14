#!/usr/bin/env bash
set -euo pipefail

# ============================================
# his-mouse-friday installer (macOS / Linux)
# ============================================
# LIVE-USER installer: installs the latest tagged release.
# For development from a local checkout use scripts/dev-setup.sh instead.
# Full setup: Go, the single hmf binary (daemon, CLI, TUI, MCP shim),
# opencode plugin + slash commands, MCP wiring, daemon start.
# Pass --no-daemon to skip starting the daemon.

NO_DAEMON=0
for arg in "$@"; do [ "$arg" = "--no-daemon" ] && NO_DAEMON=1; done

GO_VERSION="1.26.5"
MODULE_PATH="github.com/herdanis/his-mouse-friday/cmd/hmf"
REPO="Herdanis/his-mouse-friday"
BRANCH="main"
RAW_BASE="https://raw.githubusercontent.com/${REPO}/${BRANCH}"
GO_INSTALL_DIR="$HOME/.local/go"
OPENCODE_CONFIG="${OPENCODE_CONFIG:-$HOME/.config/opencode}"

info() { printf '\033[36m▸\033[0m %s\n' "$1"; }
ok()   { printf '\033[32m✓\033[0m %s\n' "$1"; }
warn() { printf '\033[33m!\033[0m %s\n' "$1"; }
die()  { printf '\033[31m✗ %s\033[0m\n' "$1" >&2; exit 1; }

# ============================================
# Detect platform
# ============================================
case "$(uname -s)" in
  Darwin) OS=darwin ;;
  Linux)  OS=linux ;;
  *) die "unsupported OS $(uname -s)" ;;
esac
case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  arm64|aarch64) ARCH=arm64 ;;
  *) die "unsupported arch $(uname -m)" ;;
esac

version_ge() { printf '%s\n%s' "$2" "$1" | sort -V -C; }

# A stale GOROOT env var (gvm or another version manager) makes `go` load its
# tools from the wrong SDK → "compile: version X does not match go tool Y".
unset GOROOT

# ============================================
# Ensure Go
# ============================================
need_go=1
if command -v go >/dev/null 2>&1; then
  have="$(go version | awk '{print $3}' | sed 's/go//')"
  if version_ge "$have" "$GO_VERSION"; then
    ok "Go $have already installed"
    need_go=0
  else
    info "Go $have is older than $GO_VERSION — installing newer"
  fi
fi

if [ "$need_go" -eq 1 ]; then
  tarball="go${GO_VERSION}.${OS}-${ARCH}.tar.gz"
  info "downloading $tarball"
  tmp="$(mktemp -d)"
  curl -fsSL "https://go.dev/dl/${tarball}" -o "$tmp/go.tar.gz" || die "download failed"
  rm -rf "$GO_INSTALL_DIR"
  mkdir -p "$(dirname "$GO_INSTALL_DIR")"
  tar -C "$(dirname "$GO_INSTALL_DIR")" -xzf "$tmp/go.tar.gz"
  rm -rf "$tmp"
  export PATH="$GO_INSTALL_DIR/bin:$PATH"
  ok "Go $GO_VERSION installed to $GO_INSTALL_DIR"
fi

# ============================================
# Install / update hmf (single binary: daemon, CLI, TUI, MCP shim)
# ============================================
latest="$(git ls-remote --tags --refs "https://github.com/${REPO}.git" 2>/dev/null |
  awk -F'refs/tags/' '{print $2}' | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | sort -V | tail -1)"
[ -n "$latest" ] || die "could not determine latest version — check access to $REPO"

current=""
if command -v hmf >/dev/null 2>&1; then
  current="$(hmf --version 2>/dev/null | awk '{print $NF}')"
fi

if [ "$current" = "$latest" ]; then
  ok "hmf $current already up to date"
else
  if [ -n "$current" ]; then
    info "updating hmf $current -> $latest"
  else
    info "installing hmf $latest"
  fi
  go clean -cache 2>/dev/null || true
  go install "${MODULE_PATH}@${latest}" || die "go install failed — check access to $REPO"
fi

gobin="$(go env GOBIN)"
[ -n "$gobin" ] || gobin="$(go env GOPATH)/bin"

# ============================================
# Opencode plugin + slash commands
# ============================================
mkdir -p "$OPENCODE_CONFIG/plugins" "$OPENCODE_CONFIG/plugins/hmf" \
         "$OPENCODE_CONFIG/commands"

fetch() { # <remote-path> <local-path>
  info "fetching $1"
  curl -fsSL "${RAW_BASE}/$1" -o "$2" || die "fetch $1 failed"
}

fetch "examples/plugins/hmf/plugin.ts"        "$OPENCODE_CONFIG/plugins/hmf/plugin.ts"
fetch "examples/commands/hmf-setup.md"        "$OPENCODE_CONFIG/commands/hmf-setup.md"
fetch "examples/commands/hmf-register.md"     "$OPENCODE_CONFIG/commands/hmf-register.md"

ok "plugin + commands → $OPENCODE_CONFIG"

# ============================================
# Path
# ============================================
case ":$PATH:" in
  *":$gobin:"*) ;;
  *) warn "add to your shell profile: export PATH=\"$gobin:\$PATH\"" ;;
esac
if [ "$need_go" -eq 1 ]; then
  warn "add to your shell profile: export PATH=\"$GO_INSTALL_DIR/bin:\$PATH\""
fi

# ============================================
# Wire MCP server + plugin into opencode config
# ============================================
# Handles both opencode.json and opencode.jsonc (comments/trailing commas)
# via tolerant text insertion — jq can't parse JSONC.
CFG="$OPENCODE_CONFIG/opencode.json"
[ -f "$CFG" ] || CFG="$OPENCODE_CONFIG/opencode.jsonc"

if [ -f "$CFG" ] && grep -q '"hmf"' "$CFG" && grep -q 'plugins/hmf/plugin.ts' "$CFG"; then
  ok "MCP server + plugin already wired in $CFG"
else
  command -v python3 >/dev/null 2>&1 || die "python3 required — wire $CFG manually (see README)"
  python3 - "$CFG" <<'PY'
import sys, os
p = sys.argv[1]
s = open(p).read() if os.path.exists(p) else '{\n  "$schema": "https://opencode.ai/config.json",\n}\n'
changed = []
if '"hmf"' not in s.split('"plugin"')[0]:
    block = '''    "hmf": {
      "command": ["hmf", "mcp"],
      "enabled": true,
      "type": "local",
    },
'''
    import re
    s2 = re.sub(r'("mcp"\s*:\s*\{)', r'\1\n' + block, s, count=1)
    if s2 == s:
        s2 = s.replace('{\n', '{\n  "mcp": {\n' + block + '  },\n', 1)
    s, changed = s2, changed + ["mcp.hmf"]
if 'plugins/hmf/plugin.ts' not in s and re.search(r'"plugin"\s*:\s*\[', s):
    import re
    s2 = re.sub(r'("plugin"\s*:\s*\[)', r'\1\n    "./plugins/hmf/plugin.ts",', s, count=1)
    s, changed = s2, changed + ["plugin entry"]
open(p, "w").write(s)
print("wired:", ", ".join(changed) if changed else "nothing")
PY
  ok "config wired → $CFG"
fi

# ============================================
# Start daemon
# ============================================
HMF_STATE="${HMF_STATE_DIR:-$HOME/.hmf}"
SOCKET_PATH="$HMF_STATE/daemon.sock"

if [ "$NO_DAEMON" -eq 1 ]; then
  info "skipping daemon start (--no-daemon)"
elif [ -S "$SOCKET_PATH" ]; then
  ok "daemon already running ($SOCKET_PATH)"
else
  info "starting daemon in background (log: $HMF_STATE/hmf.log)"
  mkdir -p "$HMF_STATE"
  nohup "$gobin/hmf" up >> "$HMF_STATE/hmf.log" 2>&1 &
  sleep 2
  if [ -S "$SOCKET_PATH" ]; then
    ok "daemon running"
  else
    warn "daemon may still be starting — verify with 'hmf status', log: $HMF_STATE/hmf.log"
  fi
fi

cat <<EOF

Next steps:
  1. Verify the daemon:        hmf status
  2. Register a project:       hmf project add <name> /path/to/repo
  3. In any registered repo:   hmf init  (creates mouse.yaml + MOUSE.md)
  4. Open the orchestrator:    hmf  (bare — TUI: threads/agents/projects/todos)

Stop the daemon later with:   hmf down
EOF

ok "done"
