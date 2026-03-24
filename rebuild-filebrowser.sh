#!/usr/bin/env bash
set -euo pipefail

# ── Root directory ─────────────────────────────────────────────────────────────
# The directory filebrowser will serve.  Change this one line to point
# at your files.  An absolute path is recommended.
ROOT_DIR="/Users/jgainor/cnc/haas"
# ──────────────────────────────────────────────────────────────────────────────

# Always run from the repo root regardless of where the script is called from
cd "$(dirname "$0")"

echo ""
echo "  Root directory: $ROOT_DIR"
echo ""

echo "=== Building frontend ==="
cd frontend
corepack pnpm install
corepack pnpm run build
cd ..

echo "=== Building Go backend ==="
go build -o filebrowser

# Write a config file so the root is picked up whether the binary is run
# directly, under systemctl, or via any other launcher — no flags needed.
# To change the root, edit ROOT_DIR at the top of this script and rebuild.
echo "=== Writing .filebrowser.yaml ==="
printf 'root: "%s"\n' "$ROOT_DIR" > .filebrowser.yaml
echo "  root: $ROOT_DIR  →  .filebrowser.yaml"

echo "=== Stopping existing filebrowser instance ==="
if [[ "$(uname)" == "Linux" ]]; then
  sudo systemctl stop filebrowser || true
else
  killall filebrowser 2>/dev/null || true
  sleep 1   # give the process time to release the db lock
fi

echo "=== Deploy ==="
if [[ "$(uname)" == "Linux" ]]; then
  sudo systemctl start filebrowser
  echo "Done. Tailing logs — ctrl+c to exit."
  sudo journalctl -u filebrowser -f
else
  echo "Non-Linux detected — skipping systemctl."
  echo "Run ./filebrowser to test locally  (root: $ROOT_DIR)"
fi