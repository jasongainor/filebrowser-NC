#!/usr/bin/env bash
set -euo pipefail

# Always run from the script's directory
cd "$(dirname "$0")"

echo "=== Building frontend ==="
cd frontend
corepack pnpm install
corepack pnpm run build
cd ..

echo "=== Building Go backend ==="
go build -o filebrowser

echo "=== Restarting filebrowser.service ==="
sudo systemctl restart filebrowser

echo "Done. Check logs with: sudo journalctl -u filebrowser -f"
