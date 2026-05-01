# Mode A: Pi presents itself as a USB mass-storage device to the connected CNC controller.
#
# Architecture:
#   - A backing image file (FAT32) at $IMAGE_PATH is what g_mass_storage exports.
#   - We loop-mount that image at $SHARE_PATH so filebrowser writes to it directly.
#     One source of truth: a write through filebrowser lands in the FAT32 image
#     immediately. No rsync. No drift between "what the user sees" and
#     "what the controller sees".
#   - On the OTG-USB side, the controller reads from the same image. When files
#     change, the watcher does a quick eject+reattach so the controller picks
#     up the new contents (most CNC firmwares cache the FAT directory).
#
# shellcheck shell=bash

# enable_dwc2 — make sure dwc2 OTG controller is enabled on this Pi.
# Edits /boot/firmware/config.txt and /boot/firmware/cmdline.txt (Bookworm path).
# Falls back to /boot/* on older Raspberry Pi OS.
enable_dwc2() {
  step "Configuring dwc2 USB OTG"

  local cfg="/boot/firmware/config.txt" cmd="/boot/firmware/cmdline.txt"
  [[ -f $cfg ]] || cfg="/boot/config.txt"
  [[ -f $cmd ]] || cmd="/boot/cmdline.txt"
  [[ -f $cfg ]] || die "could not find config.txt — is this a Raspberry Pi?"
  [[ -f $cmd ]] || die "could not find cmdline.txt — is this a Raspberry Pi?"

  if ! grep -qE '^\s*dtoverlay=dwc2' "$cfg"; then
    cp -a "$cfg" "${cfg}.cnc-pi.bak"
    printf '\n# enabled by setup-pi.sh — USB OTG for mass-storage gadget\ndtoverlay=dwc2\n' >> "$cfg"
    ok "added dtoverlay=dwc2 to $cfg (backup: ${cfg}.cnc-pi.bak)"
    # REBOOT_REQUIRED is read by setup-pi.sh.
    # shellcheck disable=SC2034
    REBOOT_REQUIRED=1
  else
    ok "dwc2 overlay already enabled in $cfg"
  fi

  if ! grep -q 'modules-load=.*dwc2' "$cmd"; then
    cp -a "$cmd" "${cmd}.cnc-pi.bak"
    # cmdline.txt is single-line; append modules-load=dwc2.
    sed -i 's/$/ modules-load=dwc2/' "$cmd"
    ok "added modules-load=dwc2 to $cmd (backup: ${cmd}.cnc-pi.bak)"
    # shellcheck disable=SC2034
    REBOOT_REQUIRED=1
  else
    ok "dwc2 module-load already set in $cmd"
  fi

  if ! grep -qE '^\s*g_mass_storage' /etc/modules 2>/dev/null; then
    # We do NOT auto-load g_mass_storage at boot via /etc/modules — the systemd
    # unit loads it with the right `file=` argument. Touching /etc/modules would
    # cause a no-args load that would fail and clutter dmesg.
    :
  fi
}

# create_backing_image <path> <size_mb>
create_backing_image() {
  local image=$1 size_mb=$2
  step "Creating backing image at $image (${size_mb} MB, FAT32)"

  if [[ -f $image ]]; then
    log "image already exists; keeping it (delete the file and re-run setup if you want a fresh one)"
    return 0
  fi

  mkdir -p "$(dirname "$image")"
  # Sparse via dd seek — fast even for large sizes.
  dd if=/dev/zero of="$image" bs=1M count=0 seek="$size_mb" status=none
  # FAT32 with a friendly volume label. -F 32 forces FAT32 even on small sizes.
  mkfs.vfat -F 32 -n CNC "$image" >/dev/null
  ok "created $image"
}

# mount_backing_image <image_path> <mount_point>
# Persists in /etc/fstab so it survives reboot.
mount_backing_image() {
  local image=$1 mount_point=$2
  step "Mounting $image at $mount_point"

  mkdir -p "$mount_point"

  # Idempotent fstab entry — keyed on mount point.
  local fstab_line="$image  $mount_point  vfat  loop,uid=1000,gid=1000,umask=000,flush  0  0"
  if grep -qE "^\s*[^#].*\s${mount_point}\s" /etc/fstab; then
    # Replace existing line for this mount point (preserves other lines).
    sed -i.cnc-pi.bak "\|[[:space:]]${mount_point}[[:space:]]|c\\${fstab_line}" /etc/fstab
    ok "updated fstab entry for $mount_point"
  else
    printf '%s\n' "$fstab_line" >> /etc/fstab
    ok "appended fstab entry for $mount_point"
  fi

  # Mount now if not mounted.
  if mountpoint -q "$mount_point"; then
    log "$mount_point already mounted — remounting to apply new options"
    mount -o remount "$mount_point" || mount "$mount_point"
  else
    mount "$mount_point"
  fi
  ok "mounted"
}

install_usb_mass_storage_mode() {
  ensure_pkgs dosfstools inotify-tools

  enable_dwc2

  create_backing_image "$IMAGE_PATH" "$IMAGE_SIZE_MB"
  mount_backing_image  "$IMAGE_PATH" "$SHARE_PATH"

  step "Installing USB-gadget systemd units"

  render_template "$REPO_DIR/pi-setup/systemd/cnc-usb-mass-storage.service.tmpl" \
                  /etc/systemd/system/cnc-usb-mass-storage.service \
                  IMAGE_PATH="$IMAGE_PATH" \
                  USB_VENDOR="$USB_VENDOR" \
                  USB_PRODUCT="$USB_PRODUCT" \
                  USB_SERIAL="$USB_SERIAL"
  ok "wrote /etc/systemd/system/cnc-usb-mass-storage.service"

  install -m 0755 "$REPO_DIR/pi-setup/scripts/cnc-usb-watcher" /usr/local/bin/cnc-usb-watcher
  ok "installed /usr/local/bin/cnc-usb-watcher"

  render_template "$REPO_DIR/pi-setup/systemd/cnc-usb-watcher.service.tmpl" \
                  /etc/systemd/system/cnc-usb-watcher.service \
                  WATCHER_BIN=/usr/local/bin/cnc-usb-watcher
  ok "wrote /etc/systemd/system/cnc-usb-watcher.service"

  enable_now cnc-usb-mass-storage.service
  enable_now cnc-usb-watcher.service
  ok "USB mass-storage gadget + watcher enabled"
}
