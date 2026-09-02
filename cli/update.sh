#!/usr/bin/env bash
# update.sh — rebuild the latest gw (and the gateway binary) from this repo and
# install them to ~/.local/bin so `gw` reflects the newest code. No sudo needed.
#
#   cli/update.sh
#
# If ~/.local/bin is not on PATH yet:
#   echo 'export PATH="$HOME/.local/bin:$PATH"' >> ~/.zshrc
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
bin_dir="${HOME}/.local/bin"
mkdir -p "$bin_dir"

echo "[1/2] building gw → $bin_dir/gw"
(cd "$root/cli" && go build -o "$bin_dir/gw" .)

echo "[2/2] building gateway → $bin_dir/gateway"
(cd "$root" && go build -o "$bin_dir/gateway" ./cmd/gateway)

echo
echo "updated. run 'gw --help' to confirm."
case ":$PATH:" in
  *":$bin_dir:"*) : ;;
  *) echo "note: add $bin_dir to PATH (e.g. export PATH=\"$bin_dir:\$PATH\")" ;;
esac
