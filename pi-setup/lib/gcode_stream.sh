# Mode B: stream G-code over a serial / USB-CDC link to a simpler CNC router.
#
# Stretch goal — not implemented yet. Prints a friendly message and exits.
# The user explicitly said this should be a stretch path; the primary target
# is the USB mass-storage flow for industrial controllers (Haas etc).
#
# When implemented, this would likely:
#   - Install something like cncjs or a minimal gcode sender daemon
#   - Expose a small "Send" button in filebrowser (would need frontend work)
#   - Map serial port via udev rule so the controller is at a stable path
#
# shellcheck shell=bash

install_gcode_stream_mode() {
  step "G-code streaming mode"
  warn "Not implemented yet — this is the stretch path."
  log ""
  log "What this WILL do (later):"
  log "  - install cncjs (or similar) as a systemd service"
  log "  - expose it on the LAN alongside filebrowser"
  log "  - point it at $SHARE_PATH so a 'send to machine' UX is possible"
  log ""
  log "For now, we're still going to set up filebrowser pointed at:"
  log "  $SHARE_PATH"
  log ""
  log "When you want streaming, re-run setup-pi.sh and pick mode 1 (USB)"
  log "or wait for this path to be filled in."
  log ""
}
