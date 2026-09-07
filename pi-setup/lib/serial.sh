# pi-setup/lib/serial.sh — stable naming + config seeding for the Haas
# USB→RS-232 adapter (docs/SERIAL_TRANSPORT.md).
#
# Why this exists:
#   cncd's serial transport (Machine.Serial.Device) wants a tty path that
#   survives reboots and re-plugs. Bare /dev/ttyUSB0 numbering is NOT
#   guaranteed to survive either — plug order, or a second USB-serial
#   device ever showing up (a second adapter, a GPS dongle, a debug
#   console), can silently swap which physical cable is ttyUSB0 vs
#   ttyUSB1. A udev rule keyed on the adapter's USB vendor/product (and
#   optionally its serial number, for telling two identical adapters
#   apart) gives a fixed symlink instead: /dev/ttyHAAS.
#
# What this file does NOT do: talk to the adapter, validate wiring, or
# touch cnc/, cncd/, or http/ Go code. It only prepares the OS-level
# path cncd's --config points a Machine.serial.device at.
#
# shellcheck shell=bash

# SERIAL_LINK_NAME — the stable device name the udev rule creates under
# /dev. Parametrized (not hardcoded "ttyHAAS" everywhere) so a shop with
# more than one direct-serial machine can run this twice with different
# names — e.g. ttyHAAS1 / ttyHAAS2 — without editing this file.
SERIAL_LINK_NAME=${SERIAL_LINK_NAME:-ttyHAAS}
SERIAL_UDEV_RULE_PATH=${SERIAL_UDEV_RULE_PATH:-/etc/udev/rules.d/99-cnc-serial.rules}

# list_serial_candidates — print one line per USB-serial adapter currently
# plugged in, as "DEVNODE  VENDOR:PRODUCT  SERIAL  MANUFACTURER MODEL".
# Read-only — no udevadm control, no writes. Safe to call any time,
# including in DRY_RUN, including with nothing plugged in (prints nothing
# and returns 1 so callers can tell "no candidates" apart from "found some").
list_serial_candidates() {
  local dev found=0
  shopt -s nullglob
  for dev in /dev/ttyUSB* /dev/ttyACM*; do
    [[ -e $dev ]] || continue
    local info vendor product serial mfr model
    info=$(udevadm info -q property -n "$dev" 2>/dev/null) || continue
    vendor=$(sed -n 's/^ID_VENDOR_ID=//p' <<<"$info")
    product=$(sed -n 's/^ID_MODEL_ID=//p' <<<"$info")
    serial=$(sed -n 's/^ID_SERIAL_SHORT=//p' <<<"$info")
    mfr=$(sed -n 's/^ID_VENDOR=//p' <<<"$info")
    model=$(sed -n 's/^ID_MODEL=//p' <<<"$info")
    [[ -n $vendor && -n $product ]] || continue
    printf '%s\t%s:%s\t%s\t%s %s\n' "$dev" "$vendor" "$product" "${serial:-(none)}" "$mfr" "$model"
    found=1
  done
  shopt -u nullglob
  (( found ))
}

# pick_serial_candidate <out-var-devnode> <out-var-vendor> <out-var-product> <out-var-serial>
# Interactive picker over list_serial_candidates. Sets the four named
# variables. Returns 1 (with a warning, no variables set) if nothing is
# plugged in — caller decides whether that's fatal.
pick_serial_candidate() {
  local __devnode=$1 __vendor=$2 __product=$3 __serial=$4
  local rows
  rows=$(list_serial_candidates) || {
    warn "no USB-serial adapter found under /dev/ttyUSB* or /dev/ttyACM* — plug in the RS-232 cable and re-run this step"
    return 1
  }

  local -a lines labels devnodes vendors products serials
  mapfile -t lines <<<"$rows"
  local line dn vp sn desc
  for line in "${lines[@]}"; do
    IFS=$'\t' read -r dn vp sn desc <<<"$line"
    devnodes+=("$dn")
    vendors+=("${vp%%:*}")
    products+=("${vp#*:}")
    serials+=("$sn")
    labels+=("$dn  ($vp, serial $sn)  $desc")
  done

  if (( ${#labels[@]} == 1 )); then
    log "one USB-serial adapter found: ${labels[0]}"
    printf -v "$__devnode" '%s' "${devnodes[0]}"
    printf -v "$__vendor"  '%s' "${vendors[0]}"
    printf -v "$__product" '%s' "${products[0]}"
    printf -v "$__serial"  '%s' "${serials[0]}"
    return 0
  fi

  local choice
  ask_choice choice "Multiple USB-serial adapters found — which one is the Haas cable?" "${labels[@]}"
  local i
  for i in "${!labels[@]}"; do
    if [[ ${labels[$i]} == "$choice" ]]; then
      printf -v "$__devnode" '%s' "${devnodes[$i]}"
      printf -v "$__vendor"  '%s' "${vendors[$i]}"
      printf -v "$__product" '%s' "${products[$i]}"
      printf -v "$__serial"  '%s' "${serials[$i]}"
      return 0
    fi
  done
  return 1
}

# write_serial_udev_rule <vendor_id> <product_id> [serial] [link_name]
# Writes a udev rule mapping the given USB vendor:product (and, if given,
# serial number — needed to disambiguate two identical adapters) to a
# stable /dev/$link_name symlink, then reloads udev so it takes effect
# without a reboot.
write_serial_udev_rule() {
  local vendor=$1 product=$2 serial=${3:-} link_name=${4:-$SERIAL_LINK_NAME}

  [[ -n $vendor && -n $product ]] || die "write_serial_udev_rule: vendor and product IDs are required"

  step "Writing udev rule for stable /dev/$link_name"

  local serial_match=""
  if [[ -n $serial ]]; then
    serial_match=", ATTRS{serial}==\"$serial\""
  fi

  local rule
  rule=$(cat <<EOF
# /etc/udev/rules.d/99-cnc-serial.rules — written by pi-setup/lib/serial.sh
# Stable name for the USB→RS-232 adapter wired to the Haas RS-232 header.
# See docs/SERIAL_TRANSPORT.md. Re-run pi-setup/setup-pi.sh's serial step
# to add another adapter or change which one this maps to.
SUBSYSTEM=="tty", ATTRS{idVendor}=="$vendor", ATTRS{idProduct}=="$product"$serial_match, SYMLINK+="$link_name", MODE="0660", GROUP="dialout"
EOF
)

  if [[ $DRY_RUN == 1 ]]; then
    log "[dry-run] would write $SERIAL_UDEV_RULE_PATH:"
    printf '%s\n' "$rule" | sed 's/^/      /'
  else
    printf '%s\n' "$rule" > "$SERIAL_UDEV_RULE_PATH"
    ok "wrote $SERIAL_UDEV_RULE_PATH"
  fi

  run udevadm control --reload-rules
  run udevadm trigger --subsystem-match=tty
  ok "udev rules reloaded — /dev/$link_name should appear once the adapter is plugged in"
}

# add_serial_group <user> — dialout membership is what lets a non-root
# cncd process open /dev/$SERIAL_LINK_NAME (mode 0660, group dialout from
# the udev rule above) without running as root. Idempotent — usermod -aG
# is a no-op if already a member.
add_serial_group() {
  local user=$1
  [[ -n $user ]] || die "add_serial_group: user is required"
  run usermod -aG dialout "$user"
  ok "$user added to the dialout group (takes effect on next login / service restart)"
}

# random_hex <nbytes> — best-effort token generator. openssl is the
# common case; /dev/urandom + od is the fallback for a minimal image
# that hasn't installed openssl yet.
random_hex() {
  local n=$1
  if command -v openssl &>/dev/null; then
    openssl rand -hex "$n"
  else
    head -c "$n" /dev/urandom | od -An -tx1 | tr -d ' \n'
  fi
}

# seed_cncd_config <config_path> <serial_device> [machine_name]
# Creates --config's JSON file with a single Machine pre-wired for direct
# serial (docs/SERIAL_TRANSPORT.md's Haas defaults — 9600 7E1 XON/XOFF —
# via settings.Machine.EffectiveSerial, so leaving baud/parity/etc. out of
# this seed is intentional, not an oversight) and a freshly generated
# machineToken.
#
# Idempotent and non-destructive: if the file already exists, this never
# overwrites it — cncd itself will have already created a zero-value file
# on first boot if nothing else did, and an operator's hand-edits (a
# second machine, a Discord token) must never be silently clobbered by a
# re-run of setup-pi.sh. In that case it just checks whether a serial
# device is already configured and tells the operator what to add by hand
# if not.
seed_cncd_config() {
  local config_path=$1 device=$2 machine_name=${3:-Haas}

  if [[ -f $config_path ]]; then
    if grep -q '"device"' "$config_path" 2>/dev/null; then
      ok "$config_path already has a serial.device configured — leaving it alone"
    else
      warn "$config_path exists without a serial.device set — add one by hand:"
      warn "  \"serial\": { \"device\": \"$device\" }  under the Machine entry in \"machines\""
    fi
    return 0
  fi

  step "Seeding $config_path with a serial-first machine"

  local machine_id token
  machine_id="m-$(random_hex 4)"
  token=$(random_hex 24)

  local content
  content=$(cat <<EOF
{
  "machines": [
    {
      "id": "$machine_id",
      "name": "$machine_name",
      "brand": "haas",
      "serial": {
        "device": "$device"
      }
    }
  ],
  "machineToken": "$token",
  "discord": {},
  "displays": [],
  "baselinePollSeconds": 15
}
EOF
)

  if [[ $DRY_RUN == 1 ]]; then
    log "[dry-run] would write $config_path:"
    printf '%s\n' "$content" | sed 's/^/      /'
    log "[dry-run] would write CNCD_MACHINE_TOKEN=<generated> to /etc/cncd/env"
    return 0
  fi

  mkdir -p "$(dirname "$config_path")"
  printf '%s\n' "$content" > "$config_path"
  chmod 0640 "$config_path"
  ok "wrote $config_path (machine \"$machine_name\", serial device $device)"

  # The machineToken above is the one source of truth cncd itself reads
  # (from config.json). We also drop a copy into /etc/cncd/env as
  # CNCD_MACHINE_TOKEN purely for shell tooling (cnc-status) that wants to
  # curl an authenticated endpoint without a JSON parser on hand. If you
  # rotate the token, update both — see pi-setup/README.md's cncd section.
  local env_path
  env_path="$(dirname "$config_path")/env"
  if [[ ! -f $env_path ]]; then
    printf '# /etc/cncd/env — sourced by cncd.service (EnvironmentFile=-) and cnc-status.\n' > "$env_path"
    printf '# CNCD_MACHINE_TOKEN must match "machineToken" in %s.\n' "$config_path" >> "$env_path"
    printf 'CNCD_MACHINE_TOKEN=%s\n' "$token" >> "$env_path"
    chmod 0600 "$env_path"
    ok "wrote $env_path (mode 0600)"
  fi
}

# install_serial_support <config_path> <service_user> [link_name]
# Top-level orchestrator: pick an adapter, write the udev rule, add the
# service user to dialout, seed the config. Called from setup-pi.sh only
# when the operator opts in (ENABLE_SERIAL=y) — a Pi with no RS-232 cable
# yet, or one that's staying on the Waveshare TCP bridge, should be able
# to skip this entirely.
install_serial_support() {
  local config_path=$1 service_user=$2 link_name=${3:-$SERIAL_LINK_NAME}

  step "Setting up direct serial (docs/SERIAL_TRANSPORT.md)"

  local devnode vendor product serial
  if ! pick_serial_candidate devnode vendor product serial; then
    if [[ $DRY_RUN == 1 ]]; then
      log "[dry-run] no adapter plugged in here — using placeholder IDs so the rest of the flow can be exercised"
      vendor=0403 product=6001 serial=""
    else
      warn "skipping serial setup — plug the USB→RS-232 adapter in and re-run setup-pi.sh"
      return 1
    fi
  fi

  ensure_pkgs udev

  write_serial_udev_rule "$vendor" "$product" "$serial" "$link_name"
  add_serial_group "$service_user"
  seed_cncd_config "$config_path" "/dev/$link_name"
}
