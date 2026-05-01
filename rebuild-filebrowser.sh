#!/usr/bin/env bash
#
# rebuild-filebrowser.sh — build the frontend + Go binary, then start the server.
#
# Reads the served folder from .filebrowser.yaml (run ./setup.sh once to create it).
# If .filebrowser.yaml is missing, falls back to ROOT_DIR_DEFAULT below — edit it
# or just run ./setup.sh.

set -euo pipefail
cd "$(dirname "$0")"

CONFIG=.filebrowser.yaml
ROOT_DIR_DEFAULT="$HOME/cnc/files"

# Pull root from .filebrowser.yaml if present, else use default.
if [[ -f $CONFIG ]]; then
  ROOT_DIR=$(awk -F'"' '/^root:/ {print $2}' "$CONFIG" 2>/dev/null || true)
fi
ROOT_DIR="${ROOT_DIR:-$ROOT_DIR_DEFAULT}"

mkdir -p "$ROOT_DIR"

echo
echo "  Root directory: $ROOT_DIR"
echo

echo "=== Building frontend ==="
cd frontend
corepack pnpm install
corepack pnpm run build
cd ..

echo "=== Building Go backend ==="
go build -o filebrowser

echo "=== Writing $CONFIG ==="
printf 'root: "%s"\n' "$ROOT_DIR" > "$CONFIG"
echo "  root: $ROOT_DIR  →  $CONFIG"

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
  exec sudo journalctl -u filebrowser -f
else
  echo "Starting filebrowser locally — ctrl+c to stop."
  echo "  → http://localhost:8080  (root: $ROOT_DIR)"
  echo
  exec ./filebrowser
fi
