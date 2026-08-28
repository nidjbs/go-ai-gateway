#!/usr/bin/env bash
# install.sh — build & install the gw CLI, then wire it into PATH.
#
# Usage:
#   cli/install.sh
#
# Prefers an install dir that is already on PATH so `gw` works from any shell
# without shell config:
#   $GW_BIN_DIR           → explicit override
#   ~/.local/bin          → when already on PATH
#   /usr/local/bin        → default on macOS; prompts for sudo when needed
# Fallback: ~/.local/bin, appending it to your shell rc and printing the
# command to activate it in the current session.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
command -v go >/dev/null || { echo "install.sh: go not found in PATH" >&2; exit 1; }

on_path() { case ":$PATH:" in *":$1:"*) return 0 ;; *) return 1 ;; esac; }

choose_bin_dir() {
  if [[ -n "${GW_BIN_DIR:-}" ]]; then
    printf '%s\n' "$GW_BIN_DIR"
    return
  fi
  if on_path "$HOME/.local/bin"; then
    printf '%s\n' "$HOME/.local/bin"
    return
  fi
  # Default: /usr/local/bin — on the system PATH on macOS, so `gw` works in
  # every shell (including sh) with no rc changes. install_binaries uses sudo
  # when the dir is not writable. Set GW_NO_SUDO=1 or GW_BIN_DIR to opt out.
  if on_path "/usr/local/bin" && [[ "${GW_NO_SUDO:-}" != "1" ]]; then
    printf '%s\n' "/usr/local/bin"
    return
  fi
  printf '%s\n' "$HOME/.local/bin"
}

# needs_build returns 0 when binaries are missing or GW_REBUILD=1 is set.
needs_build() {
  local dir=$1
  [[ "${GW_REBUILD:-}" == "1" ]] && return 0
  if [[ -x "$dir/gw" && -x "$dir/gateway" ]]; then
    return 1
  fi
  return 0
}

# probe_proxy fails fast when the default Go proxy is unreachable, so the
# user sees a fix instead of go build silently retrying a blocked download.
probe_proxy() {
  [[ -n "${GOPROXY:-}" ]] && return 0        # user pinned a proxy — trust it
  command -v curl >/dev/null || return 0
  if curl -m 4 -sI https://proxy.golang.org >/dev/null 2>&1; then
    return 0
  fi
  cat >&2 <<'EOF'

⚠  Cannot reach proxy.golang.org, which Go needs to download dependencies.
   If you are behind a firewall or in a region that blocks it, set a mirror
   and run this script again:
     export GOPROXY=https://goproxy.cn,direct
EOF
  return 1
}

# install_file copies src into dir, using sudo when the dir is not writable.
install_file() {
  local src=$1 dir=$2
  if [[ -w "$dir" ]]; then
    cp "$src" "$dir/"
    return
  fi
  if ! command -v sudo >/dev/null; then
    echo "install.sh: $dir is not writable and sudo is unavailable" >&2
    echo "  re-run with GW_BIN_DIR=\"$HOME/.local/bin\"" >&2
    exit 1
  fi
  sudo install -m 0755 "$src" "$dir/"
}

install_binaries() {
  local dir=$1
  mkdir -p "$dir" 2>/dev/null || true
  # Prebuilt binaries in dist/ (one per mac arch) avoid compilation entirely.
  local arch=""
  case "$(uname -m)" in
  arm64) arch="darwin-arm64" ;;
  x86_64) arch="darwin-amd64" ;;
  esac
  # Prebuilt binaries live under cli/dist/<arch>/.
  local prebuilt=""
  if [[ -n "$arch" && -x "$ROOT/cli/dist/$arch/gw" && -x "$ROOT/cli/dist/$arch/gateway" ]]; then
    prebuilt="$ROOT/cli/dist/$arch"
  fi

  if [[ -n "$prebuilt" ]]; then
    if needs_build "$dir"; then
      install_file "$prebuilt/gw" "$dir"
      install_file "$prebuilt/gateway" "$dir"
      echo "installed prebuilt gw → $dir/gw"
      echo "installed prebuilt gateway → $dir/gateway"
    else
      echo "binaries already installed (skip; set GW_REBUILD=1 to reinstall)"
    fi
    return
  fi

  if needs_build "$dir"; then
    probe_proxy || return 1
    echo "Building gw and gateway — first run downloads dependencies,"
    echo "compiling may take a minute. Subsequent runs skip this step."
    if [[ -w "$dir" ]]; then
      (cd "$ROOT/cli" && go build -o "$dir/gw" .)
      (cd "$ROOT" && go build -o "$dir/gateway" ./cmd/gateway)
    else
      # Directory needs elevated permissions (e.g. /usr/local/bin owned by root).
      local tmp
      tmp="$(mktemp -d)"
      trap 'rm -rf "$tmp"' RETURN
      (cd "$ROOT/cli" && go build -o "$tmp/gw" .)
      (cd "$ROOT" && go build -o "$tmp/gateway" ./cmd/gateway)
      sudo install -m 0755 "$tmp/gw" "$tmp/gateway" "$dir/"
    fi
    echo "installed gw → $dir/gw"
    echo "installed gateway → $dir/gateway"
  else
    echo "binaries already installed (skip build; set GW_REBUILD=1 to force)"
  fi
}

wire_path() {
  local dir=$1
  if on_path "$dir"; then
    echo "$dir already on PATH"
    return
  fi
  # Ensure at least the user's primary rc exists, then append idempotently.
  local primary=""
  case "${SHELL:-}" in
  *zsh*) primary="$HOME/.zshrc" ;;
  *bash*) primary="$HOME/.bash_profile" ;;
  esac
  if [[ -z "$primary" ]]; then
    primary="$HOME/.zshrc"
  fi
  mkdir -p "$(dirname "$primary")"
  touch "$primary"
  for rc in "$primary" "$HOME/.profile"; do
    if ! grep -qF "$dir" "$rc" 2>/dev/null; then
      printf '\n# added by go-ai-gateway install.sh\nexport PATH="%s:$PATH"\n' "$dir" >>"$rc"
      echo "added $dir to $rc"
    fi
  done
  echo
  echo "⚠  $dir is not on your current PATH yet."
  echo "   New terminals pick it up automatically. For THIS session run:"
  echo "   export PATH=\"$dir:\$PATH\""
}

BIN_DIR="$(choose_bin_dir)"
install_binaries "$BIN_DIR"
wire_path "$BIN_DIR"
echo
echo "Done. Next:"
echo "  gw up ~/gw.yaml"
echo "  gw trans \"hello world\""
