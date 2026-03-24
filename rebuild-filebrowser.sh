#!/usr/bin/env bash
set -euo pipefail

# Always run from the repo root regardless of where the script is called from
cd "$(dirname "$0")"

echo "=== Building frontend ==="
cd frontend
corepack pnpm install
corepack pnpm run build
cd ..

echo "=== Building Go backend ==="
go build -o filebrowser

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
  echo "Run ./filebrowser to test locally."
fi