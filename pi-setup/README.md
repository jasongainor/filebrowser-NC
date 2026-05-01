# pi-setup — CNC USB bridge installer

Turn a fresh Raspberry Pi into a USB stick the CNC controller can read,
with filebrowser-NC running on the LAN so you can drop files into it
remotely. The Pi watches for changes and automatically does an
eject + reattach so the controller picks up the new files without
the operator having to dismount/remount on the panel.

## What the script sets up

- **filebrowser-NC** as a systemd service, rooted at the share folder you pick.
- **A FAT32 image file** loop-mounted at the share folder.
  Filebrowser writes go directly into the image — there's no rsync between
  "what you uploaded" and "what the controller sees", they're the same bytes.
- **`g_mass_storage`** USB-gadget kernel module exporting that image file
  to the connected controller.
- **`cnc-usb-watcher`** — a small daemon (inotifywait + bash) that:
  - debounces file change events for `WATCH_DEBOUNCE_SECONDS` (default 8s)
  - enforces a minimum `WATCH_MIN_INTERVAL_SECONDS` (default 30s) between
    re-exports so a stream of edits doesn't flap the mount under the controller
  - on settle: writes empty string to `…/lun0/file` (eject), pauses, writes
    the image path back (reattach) — controller's USB stack re-enumerates
    with fresh contents

## Run it

On a fresh Pi (Bookworm or later, with an OTG-capable Pi: Zero, Zero 2W, 4, 5):

```bash
git clone https://github.com/jasongainor/filebrowser-NC.git
cd filebrowser-NC
./rebuild-filebrowser.sh        # builds the binary
sudo bash pi-setup/setup-pi.sh  # interactive installer
sudo reboot                      # only first run, to enable dwc2 OTG
```

Plug the Pi into the controller's USB port via the OTG-capable port (the
single USB-C on Zero 2 / Pi 4 / Pi 5; the inner micro-USB on Zero W).

## Re-running

Re-run `setup-pi.sh` any time. It reads previous answers from
`/etc/cnc-pi.conf` and pre-fills them — just hit Enter to keep, or type
a new value to change. Safe to re-run with the same answers.

To change one knob without re-prompting through everything, edit
`/etc/cnc-pi.conf` directly and `sudo systemctl restart cnc-usb-watcher`.

## Modes

| Mode | What it does | Status |
|---|---|---|
| **USB mass-storage** | Pi looks like a thumb drive to the CNC controller. | ✅ implemented |
| **G-code streaming** | Pi acts as a sender to a simpler router (cncjs etc). | 🚧 stretch — stub only |

## Defaults

| Setting | Default | Notes |
|---|---|---|
| `SHARE_PATH` | `~/cnc/files` | Where filebrowser is rooted |
| `IMAGE_PATH` | `~/cnc/cnc-usb.img` | The FAT32 image, loop-mounted at SHARE_PATH |
| `IMAGE_SIZE_MB` | `4096` | 4 GB. Only used when creating a new image |
| `WATCH_DEBOUNCE_SECONDS` | `8` | Quiet seconds before re-export |
| `WATCH_MIN_INTERVAL_SECONDS` | `30` | Min gap between two re-exports |

## Logs

```bash
journalctl -u filebrowser -f          # filebrowser web app
journalctl -u cnc-usb-watcher -f      # debounced watcher activity
journalctl -u cnc-usb-mass-storage -f # gadget module load/unload
```

## Troubleshooting

**Controller doesn't see fresh files.** Either the watcher isn't
re-exporting, or the controller's USB stack is too aggressive about
caching. Check `journalctl -u cnc-usb-watcher -f` — you should see
`re-export complete` lines after edits. Try shortening
`WATCH_DEBOUNCE_SECONDS` or lengthening `WATCH_MIN_INTERVAL_SECONDS`
if the controller is rejecting back-to-back re-mounts.

**`could not find LUN file under /sys`** in watcher logs. The
`g_mass_storage` module isn't loaded, usually because dwc2 isn't
available. Confirm with `lsmod | grep dwc2` and
`lsmod | grep g_mass_storage`. If dwc2 is missing, the dwc2 overlay
edit didn't take effect — check `/boot/firmware/config.txt` for
`dtoverlay=dwc2` and reboot.

**Filebrowser writes don't show up in the image.** Check that
`mountpoint -q "$SHARE_PATH"` returns true. If the image isn't mounted,
filebrowser is writing to a plain folder of the same name and the
controller will never see those bytes. `sudo mount "$SHARE_PATH"`.

**I want to wipe everything.** `sudo systemctl disable --now
filebrowser cnc-usb-watcher cnc-usb-mass-storage`, then delete the
unit files in `/etc/systemd/system/`, the loop image, and
`/etc/cnc-pi.conf`.

## Files installed

| Path | Purpose |
|---|---|
| `/etc/cnc-pi.conf` | All knobs in one place. Source of truth for re-runs. |
| `/etc/systemd/system/filebrowser.service` | Web file manager |
| `/etc/systemd/system/cnc-usb-mass-storage.service` | Loads `g_mass_storage` at boot |
| `/etc/systemd/system/cnc-usb-watcher.service` | The debounced watcher loop |
| `/usr/local/bin/cnc-usb-watcher` | The watcher script itself |
| `/etc/fstab` | Adds the loop-mount entry for the FAT32 image |
| `/boot/firmware/config.txt` | `dtoverlay=dwc2` line appended (backup written) |
| `/boot/firmware/cmdline.txt` | `modules-load=dwc2` appended (backup written) |
