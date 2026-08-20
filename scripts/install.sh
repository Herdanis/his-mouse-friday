#!/usr/bin/env bash
set -euo pipefail

# ============================================
# his-mouse-friday installer (macOS / Linux)
# ============================================
# Ensures Go, `go install`s hmf + hmf-mcp, then drops
# the opencode plugin + slash commands into ~/.config/opencode.

GO_VERSION="1.26.5"
MODULE_PATH="github.com/herdanis/his-mouse-friday/cmd/..."
REPO="Herdanis/his-mouse-friday"
BRANCH="main"
RAW_BASE="https://raw.githubusercontent.com/${REPO}/${BRANCH}"
GO_INSTALL_DIR="$HOME/.local/go"
OPENCODE_CONFIG="${OPENCODE_CONFIG:-$HOME/.config/opencode}"

info() { printf '\033[36m▸\033[0m %s\n' "$1"; }
ok()   { printf '\033[32m✓\033[0m %s\n' "$1"; }
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
# Install / update hmf + hmf-mcp
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
mkdir -p "$OPENCODE_CONFIG/plugins" "$OPENCODE_CONFIG/commands"

fetch() { # <remote-path> <local-path>
  info "fetching $1"
  curl -fsSL "${RAW_BASE}/$1" -o "$2" || die "fetch $1 failed"
}

fetch "examples/plugins/hmf/plugin.ts"        "$OPENCODE_CONFIG/plugins/hmf.ts"
fetch "examples/commands/hmf-setup.md"        "$OPENCODE_CONFIG/commands/hmf-setup.md"
fetch "examples/commands/hmf-register.md"     "$OPENCODE_CONFIG/commands/hmf-register.md"

ok "plugin + commands → $OPENCODE_CONFIG"

# ============================================
# Path + next-steps
# ============================================
case ":$PATH:" in
  *":$gobin:"*) ;;
  *) printf '\033[33m! add to your shell profile:\033[0m export PATH="%s:$PATH"\n' "$gobin" ;;
esac
if [ "$need_go" -eq 1 ]; then
  printf '\033[33m! add to your shell profile:\033[0m export PATH="%s/bin:$PATH"\n' "$GO_INSTALL_DIR"
fi

cat <<EOF

Next: wire the MCP server into opencode by adding this to
$OPENCODE_CONFIG/opencode.json:

  {
    "mcp": {
      "hmf": {
        "type": "local",
        "command": ["hmf-mcp"],
        "enabled": true
      }
    }
  }

Then start the daemon:  hmf up
EOF

ok "done"
