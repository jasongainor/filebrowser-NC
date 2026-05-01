#!/usr/bin/env bash
#
# setup.sh — local-dev configurator for filebrowser-NC.
#
# Run from the repo root. Prompts for a folder to serve, writes it to
# .filebrowser.yaml, then optionally builds and starts the server.
# Re-runnable: existing config is offered as the default.
#
# For the Pi USB-bridge installer (USB mass-storage gadget + watcher),
# see pi-setup/setup-pi.sh — that's a different beast.

set -euo pipefail
cd "$(dirname "$0")"

CONFIG=.filebrowser.yaml
DEFAULT_DIR="$HOME/cnc/files"

# Read existing root from .filebrowser.yaml if present.
existing_dir=""
if [[ -f $CONFIG ]]; then
  existing_dir=$(awk -F'"' '/^root:/ {print $2}' "$CONFIG" 2>/dev/null || true)
fi
default="${existing_dir:-$DEFAULT_DIR}"

echo
echo "filebrowser-NC :: local setup"
echo
read -r -p "  Folder to serve [$default]: " root_dir
root_dir="${root_dir:-$default}"
# expand ~ and trailing slashes
root_dir="${root_dir/#\~/$HOME}"
root_dir="${root_dir%/}"

mkdir -p "$root_dir"
printf 'root: "%s"\n' "$root_dir" > "$CONFIG"
echo "  root: $root_dir  →  $CONFIG"

echo
read -r -p "  Build and start filebrowser now? [Y/n]: " answer
answer=${answer:-y}

if [[ $answer =~ ^[yY] ]]; then
  exec ./rebuild-filebrowser.sh
fi

cat <<EOF

Setup saved. To build + start later, run:

  ./rebuild-filebrowser.sh

To change the served folder, re-run:

  ./setup.sh

EOF
