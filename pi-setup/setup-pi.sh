#!/usr/bin/env bash
#
# setup-pi.sh — interactive installer for the CNC-USB-bridge Pi.
#
# Single-command bring-up on a fresh Pi. Walks through:
#   1. Which service this Pi runs: cncd (headless CNC daemon, docs/CNCD.md
#      — the default) or filebrowser (the legacy Vue file manager)
#   2. Share folder on the Pi (default ~/cnc/files)
#   3. Mode:
#        a) USB mass-storage gadget — pretend to be a thumb drive to a CNC controller
#        b) G-code streaming server — stretch, stub
#   4. cncd only: direct USB→RS-232 serial to the Haas (docs/SERIAL_TRANSPORT.md)
#   5. Auto-installs build prereqs (Go always; Node 24 + corepack only for filebrowser)
#   6. Builds cncd (single Go binary) or filebrowser (frontend + Go backend)
#   7. For mode (a): dwc2 OTG, FAT32 backing image, g_mass_storage gadget,
#                    debounced eject+reattach watcher
#   8. cncd or filebrowser systemd service, points at the share, starts on boot
#   9. Optional reboot (required first run only, for dwc2 OTG to take effect)
#
# Idempotent: re-running prefills answers from /etc/cnc-pi.conf, skips
# already-installed prereqs, and is safe to re-run with the same answers.
#
# Usage:  sudo bash pi-setup/setup-pi.sh   (auto-sudo-elevates if needed)
#         DRY_RUN=1 bash pi-setup/setup-pi.sh   (print every mutating
#         command instead of running it — no root, network, or real Pi
#         needed; see pi-setup/lib/common.sh's `run` helper)

set -euo pipefail

REPO_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
LIB_DIR="$REPO_DIR/pi-setup/lib"

# shellcheck source=lib/common.sh
. "$LIB_DIR/common.sh"
# shellcheck source=lib/prereqs.sh
. "$LIB_DIR/prereqs.sh"
# shellcheck source=lib/usb_mass_storage.sh
. "$LIB_DIR/usb_mass_storage.sh"
# shellcheck source=lib/gcode_stream.sh
. "$LIB_DIR/gcode_stream.sh"
# shellcheck source=lib/smb_share.sh
. "$LIB_DIR/smb_share.sh"
# shellcheck source=lib/serial.sh
. "$LIB_DIR/serial.sh"

require_root "$@"

REBOOT_REQUIRED=0

# ── Defaults (overridden by /etc/cnc-pi.conf if present) ────────────────────
DEFAULT_USER=${SUDO_USER:-${USER}}
DEFAULT_HOME=$(getent passwd "$DEFAULT_USER" | cut -d: -f6)
SERVICE="cncd"         # cncd | filebrowser — which service this Pi runs
SHARE_PATH="${DEFAULT_HOME}/cnc/files"
MODE="usb"            # usb | stream
IMAGE_PATH="${DEFAULT_HOME}/cnc/cnc-usb.img"
IMAGE_SIZE_MB=4096    # 4 GB
WATCH_DEBOUNCE_SECONDS=8
WATCH_MIN_INTERVAL_SECONDS=30
USB_VENDOR="filebrowser-NC"
USB_PRODUCT="CNC USB"
USB_SERIAL="$(hostname | tr -d '\n')"
FB_USER="$DEFAULT_USER"
FB_DB="${DEFAULT_HOME}/.config/filebrowser/filebrowser.db"
# Branding asset dir lives next to the DB — outside SHARE_PATH on
# purpose so logos / icons never end up on the USB stick the controller
# sees. Populated from the repo's branding/ tree at install time.
FB_ASSETS_DIR="${DEFAULT_HOME}/.local/share/filebrowser/branding"
ADMIN_USER="admin"
ADMIN_PASSWORD="cncadmin1234"   # 12+ chars to satisfy upstream's minimum
ENABLE_SMB="y"                  # serve $SHARE_PATH as SMB so Finder/Explorer can mount it
SMB_GUEST="y"                   # no-auth (guest writable) — fine on a shop LAN
SMB_LEGACY="n"                  # SMB1/NT1 for a pre-NGC Haas Net Share — off by default (EternalBlue's protocol family)
ENABLE_AUTODEPLOY="y"           # poll origin/$DEPLOY_BRANCH and deploy it unattended
DEPLOY_BRANCH="master"          # branch cnc-autodeploy tracks

# cncd-only knobs (see docs/CNCD.md, docs/SERIAL_TRANSPORT.md).
CNCD_BIN="/usr/local/bin/cncd"        # not $REPO_DIR — cncd isn't an in-checkout binary like filebrowser
CNCD_CONFIG_DIR="/etc/cncd"
CNCD_CONFIG="${CNCD_CONFIG_DIR}/config.json"
CNCD_LISTEN=":8080"
ENABLE_SERIAL="y"      # wire up the USB→RS-232 udev rule + seed config.json's serial.device
SERIAL_LINK_NAME="ttyHAAS"

# Load existing config if present (overrides the defaults above).
load_conf || true

step "filebrowser-NC :: Pi setup"
log "This will configure cncd or filebrowser + (optionally) USB mass-storage gadget."
log "Existing config: ${CONF_PATH} ($([[ -e $CONF_PATH ]] && echo found || echo none))"

# ── Prompts ─────────────────────────────────────────────────────────────────

ask_choice SERVICE_LABEL "Which service should this Pi run?" \
  "cncd — headless CNC daemon (recommended; serial support, no bolt DB/Vue frontend)" \
  "filebrowser — legacy web file manager (being phased out)"
case $SERVICE_LABEL in
  cncd*)        SERVICE=cncd ;;
  filebrowser*) SERVICE=filebrowser ;;
esac

ask SHARE_PATH "Path on the Pi where CNC files live" "$SHARE_PATH"

ask_choice MODE_LABEL "Pick a mode" \
  "USB mass-storage (Pi acts as a thumb drive to the controller)" \
  "G-code streaming server (stretch — not implemented)"
case $MODE_LABEL in
  USB*)   MODE=usb ;;
  G-code*) MODE=stream ;;
esac

if [[ $MODE == usb ]]; then
  ask IMAGE_PATH               "Path to the FAT32 backing image" "$IMAGE_PATH"
  ask IMAGE_SIZE_MB            "Image size (MB) — only used if creating new" "$IMAGE_SIZE_MB"
  ask WATCH_DEBOUNCE_SECONDS   "Debounce — quiet seconds before re-export" "$WATCH_DEBOUNCE_SECONDS"
  ask WATCH_MIN_INTERVAL_SECONDS \
                               "Min seconds between two re-exports (no flapping)" "$WATCH_MIN_INTERVAL_SECONDS"
  ask USB_VENDOR  "USB vendor string"   "$USB_VENDOR"
  ask USB_PRODUCT "USB product string"  "$USB_PRODUCT"
  ask USB_SERIAL  "USB serial number"   "$USB_SERIAL"
fi

if [[ $SERVICE == cncd ]]; then
  ask FB_USER "User cncd will run as" "$FB_USER"
  ask CNCD_LISTEN "Address for cncd to listen on" "$CNCD_LISTEN"
  ask_yes_no ENABLE_SERIAL "Set up direct USB→RS-232 serial to the Haas (docs/SERIAL_TRANSPORT.md)?" "${ENABLE_SERIAL:-y}"
  if [[ $ENABLE_SERIAL == y ]]; then
    ask SERIAL_LINK_NAME "Stable /dev name for the adapter" "$SERIAL_LINK_NAME"
  fi
else
  ask FB_USER "User filebrowser will run as" "$FB_USER"
fi

# Admin / Samba password — set deterministically so the user knows what it
# is from the moment setup ends. Must be at least 12 chars (filebrowser's
# upstream minimum; kept the same for cncd/Samba so there's one rule to
# remember). Default is fine for shop-LAN use; change it after setup.
while :; do
  if [[ $SERVICE == cncd ]]; then
    ask ADMIN_PASSWORD "Samba / shared user password (min 12 chars)" "$ADMIN_PASSWORD"
  else
    ask ADMIN_PASSWORD "Filebrowser admin password (min 12 chars)" "$ADMIN_PASSWORD"
  fi
  (( ${#ADMIN_PASSWORD} >= 12 )) && break
  warn "must be at least 12 characters"
done

# SMB share — Pi shows up under "Network" in Finder / Explorer with the
# share folder mountable as a regular drive. Reuses the filebrowser admin
# password for the SMB user, so you don't need to remember two.
ask_yes_no ENABLE_SMB "Expose share over SMB (Finder / Explorer network drive)?" "${ENABLE_SMB:-y}"
if [[ $ENABLE_SMB == y ]]; then
  # Guest mode = no password prompt when mounting. Right answer for a
  # shop-LAN appliance; wrong answer if the box is exposed to a network
  # you don't trust.
  ask_yes_no SMB_GUEST "Allow SMB guest access (no password)?" "${SMB_GUEST:-y}"
  # Pre-NGC Haas controls (the "Net Share" tab on a Classic control) speak
  # ONLY SMB1/NT1. Samba has defaulted to SMB2_02 minimum since 4.11, so a
  # stock share is invisible to the machine. Answer y ONLY if a legacy
  # controller has to mount this share — SMB1 is the protocol family behind
  # EternalBlue/WannaCry and should not be on by default.
  ask_yes_no SMB_LEGACY "Enable SMB1/NT1 for a pre-NGC Haas Net Share?" "${SMB_LEGACY:-n}"
fi

# Unattended deploys. Worth being explicit that this means anything reaching
# the branch lands on the machine next to the mill within minutes — the
# health check reverts a binary that won't start, but not one that starts and
# misbehaves. Say no here and cnc-rebuild remains the manual path.
ask_yes_no ENABLE_AUTODEPLOY "Auto-deploy new commits from origin/${DEPLOY_BRANCH:-master}?" "${ENABLE_AUTODEPLOY:-y}"

# ── Resolve binary location ────────────────────────────────────────────────
FB_BIN="$REPO_DIR/filebrowser"
FB_WORKDIR="$REPO_DIR"

# ── Persist config ──────────────────────────────────────────────────────────
step "Saving configuration to $CONF_PATH"
write_conf \
  "SERVICE=$SERVICE" \
  "SHARE_PATH=$SHARE_PATH" \
  "MODE=$MODE" \
  "IMAGE_PATH=$IMAGE_PATH" \
  "IMAGE_SIZE_MB=$IMAGE_SIZE_MB" \
  "WATCH_DEBOUNCE_SECONDS=$WATCH_DEBOUNCE_SECONDS" \
  "WATCH_MIN_INTERVAL_SECONDS=$WATCH_MIN_INTERVAL_SECONDS" \
  "USB_VENDOR=$USB_VENDOR" \
  "USB_PRODUCT=$USB_PRODUCT" \
  "USB_SERIAL=$USB_SERIAL" \
  "FB_USER=$FB_USER" \
  "FB_BIN=$FB_BIN" \
  "FB_WORKDIR=$FB_WORKDIR" \
  "FB_DB=$FB_DB" \
  "FB_ASSETS_DIR=$FB_ASSETS_DIR" \
  "ADMIN_USER=$ADMIN_USER" \
  "ADMIN_PASSWORD=$ADMIN_PASSWORD" \
  "ENABLE_SMB=$ENABLE_SMB" \
  "SMB_GUEST=$SMB_GUEST" \
  "SMB_LEGACY=$SMB_LEGACY" \
  "ENABLE_AUTODEPLOY=$ENABLE_AUTODEPLOY" \
  "DEPLOY_BRANCH=$DEPLOY_BRANCH" \
  "CNCD_BIN=$CNCD_BIN" \
  "CNCD_CONFIG_DIR=$CNCD_CONFIG_DIR" \
  "CNCD_CONFIG=$CNCD_CONFIG" \
  "CNCD_LISTEN=$CNCD_LISTEN" \
  "ENABLE_SERIAL=$ENABLE_SERIAL" \
  "SERIAL_LINK_NAME=$SERIAL_LINK_NAME"
# Conf has the admin password — restrict to root.
chmod 0600 "$CONF_PATH" 2>/dev/null || true

# ── Build prereqs + service binary (slow, automated) ────────────────────────
# Done after prompts so the user can walk away while the install runs, and
# before the systemd unit step so we know the binary exists when we enable it.
install_build_prereqs "$SERVICE"
case $SERVICE in
  cncd)         build_cncd ;;
  filebrowser)  build_filebrowser ;;
  *)            die "unknown SERVICE=$SERVICE" ;;
esac

# ── Diagnostics one-shot script ─────────────────────────────────────────────
# Drop in early so it's available even if the rest of setup fails midway.
step "Installing cnc-status diagnostic script"
run install -m 0755 "$REPO_DIR/pi-setup/scripts/cnc-status" /usr/local/bin/cnc-status
ok "cnc-status installed → run \`cnc-status\` any time for a one-shot diagnostic dump"

step "Installing cnc-rebuild deploy script"
run install -m 0755 "$REPO_DIR/pi-setup/scripts/cnc-rebuild" /usr/local/bin/cnc-rebuild
ok "cnc-rebuild installed → run \`cnc-rebuild\` to pull, rebuild, and restart in one shot"

step "Installing cnc-autodeploy script"
run install -m 0755 "$REPO_DIR/pi-setup/scripts/cnc-autodeploy" /usr/local/bin/cnc-autodeploy
ok "cnc-autodeploy installed → the timer below runs it; cnc-rebuild stays the manual path"

# ── Mode-specific install ───────────────────────────────────────────────────
case $MODE in
  usb)    install_usb_mass_storage_mode ;;
  stream) install_gcode_stream_mode ;;
  *)      die "unknown MODE=$MODE" ;;
esac

# ── service install (last, so it sees the mounted share) ────────────────────
if [[ $SERVICE == cncd ]]; then
  step "Installing cncd systemd service"
  run mkdir -p "$SHARE_PATH"
  run install -d -m 0750 -o "$FB_USER" -g "$FB_USER" "$CNCD_CONFIG_DIR"

  if [[ -f $REPO_DIR/cncd.bin ]]; then
    run install -m 0755 "$REPO_DIR/cncd.bin" "$CNCD_BIN"
    ok "installed $CNCD_BIN"
  elif [[ $DRY_RUN != 1 ]]; then
    warn "skipping cncd binary install — $REPO_DIR/cncd.bin not built (build step failed?)"
  fi

  if [[ ${ENABLE_SERIAL:-y} == y ]]; then
    install_serial_support "$CNCD_CONFIG" "$FB_USER" "$SERIAL_LINK_NAME"
  else
    log "serial setup skipped — machine stays on the Waveshare TCP bridge until you add a serial block by hand (docs/SERIAL_TRANSPORT.md)"
  fi

  # seed_cncd_config (called from install_serial_support, or by cncd itself
  # on first start if ENABLE_SERIAL=n) writes as root; the service runs as
  # FB_USER and needs to be able to read AND rewrite these files.
  run chown -R "$FB_USER:$FB_USER" "$CNCD_CONFIG_DIR" || true
  # config.json/env may not exist yet if ENABLE_SERIAL=n — cncd creates
  # config.json itself on first start in that case, so a missing file here
  # is not an error.
  [[ -f $CNCD_CONFIG ]] && { run chmod 0640 "$CNCD_CONFIG" || true; }
  [[ -f $CNCD_CONFIG_DIR/env ]] && { run chmod 0600 "$CNCD_CONFIG_DIR/env" || true; }

  render_template "$REPO_DIR/pi-setup/systemd/cncd.service.tmpl" \
                  /etc/systemd/system/cncd.service \
                  CNCD_USER="$FB_USER" \
                  CNCD_BIN="$CNCD_BIN" \
                  ROOT="$SHARE_PATH" \
                  CONFIG="$CNCD_CONFIG" \
                  LISTEN="$CNCD_LISTEN"
  ok "wrote /etc/systemd/system/cncd.service"

  if [[ -x $CNCD_BIN || $DRY_RUN == 1 ]]; then
    enable_now cncd.service
    ok "cncd running"
  else
    run systemctl daemon-reload
    warn "skipping cncd start — binary not installed at $CNCD_BIN"
  fi

else
  step "Installing filebrowser systemd service"
  run mkdir -p "$(dirname "$FB_DB")"
  run chown -R "$FB_USER:$FB_USER" "$(dirname "$FB_DB")" || true
  run mkdir -p "$SHARE_PATH"

  render_template "$REPO_DIR/pi-setup/systemd/filebrowser.service.tmpl" \
                  /etc/systemd/system/filebrowser.service \
                  FB_USER="$FB_USER" \
                  FB_BIN="$FB_BIN" \
                  FB_WORKDIR="$FB_WORKDIR" \
                  SHARE_PATH="$SHARE_PATH" \
                  FB_DB="$FB_DB"
  ok "wrote /etc/systemd/system/filebrowser.service"

  # Pre-create the admin user with the chosen password so the box is usable
  # the moment the service starts. Idempotent on re-runs (updates the
  # password if the user already exists) — no journal-grepping for a
  # random string.
  #
  # filebrowser's BoltDB is single-writer; if the service is running we'd
  # silently lose the update and ship a "wrong" password to the user. Stop
  # the service first, then let the normal enable_now restart it.
  ensure_admin_user() {
    if [[ ! -x $FB_BIN ]]; then
      return 0
    fi
    step "Configuring filebrowser admin user"
    systemctl stop filebrowser.service 2>/dev/null || true

    # Make sure the DB schema exists (config init is a no-op if it already does).
    runuser -u "$FB_USER" -- "$FB_BIN" config init --database "$FB_DB" >/dev/null 2>&1 || true
    # Try update first (covers re-runs where admin already exists). Capture
    # stderr so a real failure surfaces instead of dying behind /dev/null.
    local out
    if out=$(runuser -u "$FB_USER" -- "$FB_BIN" users update "$ADMIN_USER" \
                     --password "$ADMIN_PASSWORD" --database "$FB_DB" 2>&1); then
      ok "updated admin password"
    elif out=$(runuser -u "$FB_USER" -- "$FB_BIN" users add "$ADMIN_USER" \
                       "$ADMIN_PASSWORD" --perm.admin --database "$FB_DB" 2>&1); then
      ok "created admin user"
    else
      warn "could not create or update admin user:"
      printf '%s\n' "$out" | sed 's/^/    /' >&2
      warn "first start will auto-init with a random password (look in journalctl -u filebrowser)"
    fi
  }
  ensure_admin_user

  # Branding assets — keep logos / icons OUT of $SHARE_PATH (and therefore
  # off the USB stick the controller sees). filebrowser supports a
  # branding-files override that points at any directory containing
  # img/<name>.{svg,png}, so we drop the repo's branding/ tree at
  # $FB_ASSETS_DIR/img/ and tell filebrowser to read from there.
  ensure_branding_assets() {
    if [[ ! -x $FB_BIN ]]; then
      return 0
    fi
    step "Installing branding assets at $FB_ASSETS_DIR"
    systemctl stop filebrowser.service 2>/dev/null || true

    install -d -m 0755 -o "$FB_USER" -g "$FB_USER" "$FB_ASSETS_DIR"
    install -d -m 0755 -o "$FB_USER" -g "$FB_USER" "$FB_ASSETS_DIR/img"
    # -n: never overwrite a file the user has customized in place.
    if [[ -d $REPO_DIR/branding ]]; then
      cp -an "$REPO_DIR"/branding/*.svg "$REPO_DIR"/branding/*.png \
         "$FB_ASSETS_DIR/img/" 2>/dev/null || true
      chown -R "$FB_USER:$FB_USER" "$FB_ASSETS_DIR" 2>/dev/null || true
    fi

    # Point filebrowser at the new dir. config set is idempotent.
    if runuser -u "$FB_USER" -- "$FB_BIN" config set \
         --branding.files "$FB_ASSETS_DIR" --database "$FB_DB" >/dev/null 2>&1; then
      ok "branding.files = $FB_ASSETS_DIR"
    else
      warn "could not set branding.files — assets installed but filebrowser config not updated"
    fi
  }
  ensure_branding_assets

  if [[ -x $FB_BIN ]]; then
    enable_now filebrowser.service
    ok "filebrowser running"
  else
    run systemctl daemon-reload
    warn "skipping filebrowser start — binary not built (build step failed?)"
  fi
fi

# ── Unattended deploys (after the service, so the health check has a target) ─
if [[ ${ENABLE_AUTODEPLOY:-y} == y ]]; then
  step "Installing cnc-autodeploy timer"
  render_template "$REPO_DIR/pi-setup/systemd/cnc-autodeploy.service.tmpl" \
                  /etc/systemd/system/cnc-autodeploy.service \
                  SERVICE="$SERVICE" \
                  FB_USER="$FB_USER" \
                  FB_WORKDIR="$FB_WORKDIR" \
                  GO_BIN="${GO_BIN:-/usr/local/go/bin/go}" \
                  DEPLOY_BRANCH="${DEPLOY_BRANCH:-master}" \
                  AUTODEPLOY_BIN=/usr/local/bin/cnc-autodeploy \
                  CNCD_BIN="$CNCD_BIN"
  render_template "$REPO_DIR/pi-setup/systemd/cnc-autodeploy.timer.tmpl" \
                  /etc/systemd/system/cnc-autodeploy.timer \
                  DEPLOY_BRANCH="${DEPLOY_BRANCH:-master}"
  enable_now cnc-autodeploy.timer
  ok "auto-deploy on — origin/${DEPLOY_BRANCH:-master} deploys within 5 min of landing"
else
  systemctl disable --now cnc-autodeploy.timer 2>/dev/null || true
  ok "auto-deploy off — use \`cnc-rebuild\` to deploy by hand"
fi

# ── SMB share (after the service, so $SHARE_PATH is owned + populated) ──────
if [[ ${ENABLE_SMB:-y} == y ]]; then
  install_smb_share
fi

# ── Done ────────────────────────────────────────────────────────────────────
step "Done"
log ""
PI_IP=$(hostname -I | awk '{print $1}')
PI_HOST=$(hostname)
if [[ $SERVICE == cncd ]]; then
  cncd_port=${CNCD_LISTEN##*:}
  log "cncd:"
  log "  Listen:   $PI_IP$CNCD_LISTEN"
  log "  Health:   curl http://$PI_IP:${cncd_port:-8080}/healthz"
  log "  Config:   $CNCD_CONFIG"
  log "  Secrets:  $CNCD_CONFIG (machineToken), $CNCD_CONFIG_DIR/env (CNCD_MACHINE_TOKEN + any bot tokens)"
  if [[ ${ENABLE_SERIAL:-y} == y ]]; then
    log "  Serial:   /dev/$SERIAL_LINK_NAME (see docs/SERIAL_TRANSPORT.md for the Haas settings that must match)"
  fi
else
  log "Filebrowser:"
  log "  URL:      http://$PI_IP:8080"
  log "  Username: $ADMIN_USER"
  log "  Password: $ADMIN_PASSWORD"
fi
log ""
if [[ ${ENABLE_SMB:-y} == y ]]; then
  log "SMB share (Mac Finder / Windows Explorer):"
  log "  Finder:   smb://$PI_HOST.local/cnc   (or smb://$PI_IP/cnc)"
  log "  Explorer: \\\\$PI_HOST\\cnc            (or \\\\$PI_IP\\cnc)"
  if [[ ${SMB_GUEST:-y} == y ]]; then
    log "  Auth:     guest (no password)"
  else
    log "  Username: $FB_USER"
    log "  Password: $ADMIN_PASSWORD"
  fi
  log ""
fi
if [[ $SERVICE == filebrowser ]]; then
  log "Change the password from the user menu once you're logged in (or"
  log "re-run this script to set a new one)."
  log ""
fi
log "Re-run this script any time to change the share folder, mode, or timings."
log ""
log "Logs:"
if [[ $SERVICE == cncd ]]; then
  log "  cncd:               journalctl -u cncd -f"
else
  log "  filebrowser:        journalctl -u filebrowser -f"
fi
log "  USB watcher:        journalctl -u cnc-usb-watcher -f"
log "  USB mass-storage:   journalctl -u cnc-usb-mass-storage -f"
log ""
if (( REBOOT_REQUIRED )); then
  warn "A reboot is required for dwc2 OTG changes to take effect."
  ask_yes_no REBOOT_NOW "Reboot now?" y
  if [[ $REBOOT_NOW == y ]]; then
    log "rebooting…"
    systemctl reboot
  else
    warn "Reboot when ready:  sudo reboot"
  fi
fi
