# pi-setup — CNC Pi installer

A Pi that pretends to be a USB stick to your CNC controller, with a web
service on the LAN as the upload UI — either **cncd** (the headless CNC
daemon, `docs/CNCD.md` — recommended for new installs, and the only path
with direct-serial support) or **filebrowser-NC** (the legacy Vue file
manager, being phased out). `setup-pi.sh` asks which one to install via
`SERVICE=cncd|filebrowser`; everything else in this doc — the USB
mass-storage gadget, the Samba share, the autodeploy timer — works the
same under either.

**The pain this solves.** When the operator changes a file, the controller
won't see it without a manual unmount + remount on the panel — and on
some controllers, even that doesn't refresh the directory cache. This
installer wires up a debounced eject + reattach, so any file write shows
up on the machine's screen a few seconds later with no panel input.

## cncd (recommended)

See `docs/CNCD.md` for what cncd actually serves, `docs/SERIAL_TRANSPORT.md`
for the direct-RS-232 story this section leans on, and `docs/JOB_FOLDERS.md`
for what actually lives under `$SHARE_PATH`.

**`SHARE_PATH` IS the jobs root** — cncd buckets it into one folder per job
directly, so there's no extra top-level folder (no `cncFiles/jobs/`, no
`_INBOX`) to click through on the control. Point the Fusion post's output
path at `$SHARE_PATH` itself (over the network share it resolves to, e.g.
`\\pi\cnc\`); cncd's auto-bucket watcher files each posted program into its
job folder within a few seconds on its own. This is on by default — see
`docs/JOB_FOLDERS.md`'s "Turning it off" if a shop wants the pre-job-folders
flat layout back.

### Fresh-Pi steps

```bash
git clone https://github.com/jasongainor/filebrowser-NC.git
cd filebrowser-NC
sudo bash pi-setup/setup-pi.sh   # SERVICE=cncd is the default — just hit Enter
sudo reboot                      # only if you picked USB mass-storage mode
```

The prompts that matter for cncd:

- **Which service** → cncd (default).
- **User cncd will run as** → whatever non-root user should own the
  share and the serial device; this user is added to the `dialout`
  group automatically.
- **Set up direct USB→RS-232 serial to the Haas?** → yes if the
  USB-to-RS232 cable is already plugged in (pick it from the list if
  more than one adapter shows up); no if this machine is staying on the
  Waveshare TCP bridge for now. Re-run setup any time to add it later.
- **Samba / shared user password** → also becomes the SMB login if you
  enable SMB below; cncd itself has no login of its own yet
  (docs/CNCD.md's "Not here yet" section).

### The three files that hold secrets

| File | Contains | Notes |
|---|---|---|
| `/etc/cncd/config.json` | `machineToken` (the bearer every external client — renishaw-builder, the e-paper firmware — presents), machine list, Discord bot token if configured | Written by `pi-setup/lib/serial.sh`'s `seed_cncd_config` on first install if you opted into serial setup; otherwise cncd creates a zero-value file itself on first start. `chmod 0640`, owned by the service user. |
| `/etc/cncd/env` | `CNCD_MACHINE_TOKEN` (a copy of `config.json`'s `machineToken`, for shell tools like `cnc-status` that don't want to parse JSON just to build an `Authorization` header), and anywhere you'd add `GMW_MES_BOT_TOKEN` or similar shared secrets | Sourced by `cncd.service` via `EnvironmentFile=-` (leading `-` = missing file is not an error). `chmod 0600`. **If you rotate the token, update both files** — cncd only reads `config.json`, `cnc-status` only reads this one. |
| `/etc/cnc-pi.conf` | `ADMIN_PASSWORD` (the Samba / shared-user password set during setup) | Same file filebrowser installs always used; `chmod 0600`. |

### Creating a Samba user by hand

`setup-pi.sh` runs this for you when you enable SMB, but if you ever
need to add or reset one manually:

```bash
sudo smbpasswd -a <username>   # -a = add; omit -a to just change the password
sudo smbpasswd -e <username>   # make sure the account isn't disabled
```

### Picking the serial adapter

`pi-setup/lib/serial.sh` lists every `/dev/ttyUSB*` / `/dev/ttyACM*`
device with its USB vendor:product and serial number
(`udevadm info -q property -n <dev>`) and, if more than one is plugged
in, asks which is the Haas cable. It then writes a udev rule pinning
that exact adapter (vendor/product, plus serial number if you have two
identical ones) to a stable `/dev/ttyHAAS`, so a reboot or a replugged
cable can't silently swap which tty the Machine's `serial.device` points
at. Run it again (re-run `setup-pi.sh`, answer yes to the serial
question) any time you swap adapters.

To find an adapter's IDs by hand instead:

```bash
udevadm info -q property -n /dev/ttyUSB0 | grep -E 'ID_VENDOR_ID|ID_MODEL_ID|ID_SERIAL_SHORT'
```

### Migrating an existing filebrowser Pi to cncd

1. **Same share path.** cncd's `--root` should point at the exact
   `$SHARE_PATH` filebrowser was already serving — nothing about the
   USB mass-storage gadget or Samba share changes.
2. **Export the old settings.** With the old filebrowser instance still
   running, as an admin:
   - `GET /api/cnc/settings` returns `{"machines": [...], "machineToken": "..."}`
     — the exact `machines` + `machineToken` shape cncd's `config.json`
     wants.
   - `GET /api/cnc/displays` returns the `displays` list, if any.
   - The Discord bot token is write-only over HTTP (masked on every
     GET) — pull it from wherever you originally set it up (the
     Discord developer dashboard, or your own notes) rather than trying
     to read it back from filebrowser.
3. **Assemble `/etc/cncd/config.json`** from those three pieces —
   `machines`, `machineToken`, `discord`, `displays` — matching the
   shape in `docs/CNCD.md`.
4. **Re-run `setup-pi.sh`, choosing `SERVICE=cncd`.** It reuses your
   existing `/etc/cnc-pi.conf` answers (share path, USB mode, SMB
   settings) and installs `cncd.service` alongside — the old
   `filebrowser.service` unit is left in place but no longer enabled by
   this script; disable it by hand once you've confirmed cncd is
   serving correctly: `sudo systemctl disable --now filebrowser`.
5. Point renishaw-builder and the e-paper firmware's base URL at the
   same `http://<pi-host>:8080` — the routes and the bearer scheme are
   unchanged (docs/CNCD.md's routes table).

### Hardware note: the Pi 4's OTG port

USB mass-storage gadget mode needs the SoC's USB OTG controller in
peripheral mode, which on a Raspberry Pi 4 Model B is wired to the
**USB-C port that also supplies power** — there is no second, separate
OTG-capable port. That means the cable to the CNC controller and the
Pi's power feed want the same physical port. Options: power the Pi from
the GPIO header (5V/GND pins) instead of USB-C, or use a USB-C splitter
cable that carries both power and data. A Compute Module 4 or a
Raspberry Pi 5 doesn't have this constraint — check the board's USB
controller wiring before assuming a spare port will do OTG.

## How it fits together

- **filebrowser-NC** runs as a systemd service rooted at the share folder.
  The share folder is a regular Linux directory — **not** a mount of
  anything.
- **A FAT32 image file** lives next to the share. It's the file
  `g_mass_storage` exports to the controller as a USB drive. Linux
  **never** loop-mounts it. The host only reads/writes the image
  through `mtools`, and only while the LUN is detached.
- **`cnc-usb-watcher`** orchestrates the bidirectional sync. On every
  file change in the share folder, after `WATCH_DEBOUNCE_SECONDS` of
  quiescence and at least `WATCH_MIN_INTERVAL_SECONDS` since the last
  cycle:
  1. Detach the LUN — controller's USB stack sees the stick unplug.
  2. Pull any controller-side new files into the share folder
     (DPRNT logs, output files the machine wrote).
  3. Atomically rebuild the image from the share (build to a temp
     `.new`, swap into place).
  4. Reattach the LUN — controller re-mounts and sees the new contents.

### Why this design

The kernel's `Documentation/usb/mass-storage.rst` is explicit:

> If the file is opened for both reading and writing and is accessed
> via the host and via the local Linux system at the same time then
> the contents of the file may be corrupted.

An earlier version of this fork loop-mounted the image AND exported
it as read-write USB. Both sides cached the FAT independently and
fought, corrupting directory entries and crossing file contents. We
had files come back garbled.

The current design sidesteps the race entirely: only one side ever
touches the image at a time. While the LUN is attached, the
controller is the only writer (host doesn't touch the image at all).
While the LUN is detached, the host syncs through `mtools` (the
controller literally can't see the device). No shared cache, no
fight.

### Conflict policy

| Situation | Outcome |
|---|---|
| File on both sides | Filebrowser wins — the rebuild from the share overwrites whatever the controller wrote |
| File only on the controller, not in last snapshot | Pulled into the share (controller-created) |
| File only on the controller, *was* in last snapshot | Dropped — the host deleted it, deletion sticks |
| File only on the host | Pushed into image via the rebuild |

The watcher keeps a snapshot of the image's contents at the last
successful sync (`/var/lib/cnc-usb-watcher/last_sync_listing`) so it
can tell "controller created a new file" apart from "host deleted a
file that was previously on the stick".

## First run

Fresh Pi, Bookworm or later, OTG-capable hardware (Zero / Zero 2 W / 4 / 5):

```bash
git clone https://github.com/jasongainor/filebrowser-NC.git
cd filebrowser-NC
./rebuild-filebrowser.sh        # builds the binary
sudo bash pi-setup/setup-pi.sh  # interactive installer
sudo reboot                     # first run only — enables dwc2 OTG
```

After reboot, plug the Pi into the controller's USB-OTG port (USB-C on
Zero 2 / Pi 4 / Pi 5 — see the OTG-port note under "cncd" above if that's
also your power port; inner micro-USB on Zero W). Whichever service you
picked is on `http://<pi-ip>:8080`.

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
| `SERVICE` | `cncd` | `cncd` or `filebrowser` — which service `setup-pi.sh` builds and installs. |
| `SHARE_PATH` | `~/cnc/files` | Regular folder the service serves (`--root` for cncd). Avoid spaces in the path. |
| `IMAGE_PATH` | `~/cnc/cnc-usb.img` | FAT32 image exported as USB. Never mounted on the host. |
| `IMAGE_SIZE_MB` | `4096` | 4 GB. Only used when creating a new image |
| `WATCH_DEBOUNCE_SECONDS` | `8` | Quiet seconds before re-export |
| `WATCH_MIN_INTERVAL_SECONDS` | `30` | Min gap between two re-exports |
| `CNCD_LISTEN` | `:8080` | cncd's listen address. |
| `ENABLE_SERIAL` | `y` | Wire up the USB→RS-232 udev rule + seed `config.json`'s `serial.device` (cncd only). |
| `SERIAL_LINK_NAME` | `ttyHAAS` | Stable `/dev/` name the udev rule creates. |

## Logs

```bash
journalctl -u cncd -f                 # cncd (SERVICE=cncd, the default)
journalctl -u filebrowser -f          # filebrowser web app (SERVICE=filebrowser)
journalctl -u cnc-usb-watcher -f      # debounced watcher activity
journalctl -u cnc-usb-mass-storage -f # gadget module load/unload
```

`cnc-status` (installed to `/usr/local/bin/cnc-status`) rolls all of this
plus `/healthz`, an authenticated `/api/cnc/state` check, serial-adapter
presence, and the USB/Samba checks below into one paste-able dump.

## Troubleshooting

**Controller doesn't see fresh files.** Check `journalctl -u
cnc-usb-watcher -f` — you should see `sync complete` followed by
`LUN reattached` after edits. If you don't, the watcher isn't being
triggered. If you do, the controller's USB stack may be caching too
aggressively; some Haas controllers need the operator to actually go
to the directory listing screen for the re-mount to register.

**`could not find LUN file under /sys`** in watcher logs. The
`g_mass_storage` module isn't loaded, usually because dwc2 isn't
available. Confirm with `lsmod | grep dwc2` and
`lsmod | grep g_mass_storage`. If dwc2 is missing, the dwc2 overlay
edit didn't take effect — check `/boot/firmware/config.txt` for
`dtoverlay=dwc2` and reboot.

**Files in filebrowser don't show up on the controller.** Verify
the share folder is *not* a mount point: `mountpoint -q $SHARE_PATH`
should return non-zero (it's just a folder now). If it IS a mount
point, you're on a stale v1 install — re-run `setup-pi.sh`, the
migration step will umount and clean up the fstab line.

**Path with spaces breaks setup (legacy).** v1 wrote an unescaped
fstab entry which would parse-fail on paths like
`/home/admin/Desktop/cnc files`. v2 doesn't write any fstab entry, so
spaces in the share path are now fine — but if you upgraded from a
half-broken v1 install, the migration step in `setup-pi.sh` removes
the bad fstab line for you.

**I want to wipe everything.** `sudo systemctl disable --now
filebrowser cnc-usb-watcher cnc-usb-mass-storage`, then delete the
unit files in `/etc/systemd/system/`, the image file, the share
folder, `/etc/cnc-pi.conf`, and `/var/lib/cnc-usb-watcher/`.

## Optional: go2rtc — persistent UniFi Protect / RTSP camera embed

UniFi Protect's "Share Live View" links default to 24-hour expiry on
UniFi OS 3.x and earlier; pasting one into Settings → Machine →
Camera URL works for a day, then breaks. Browsers also can't play
raw RTSP/RTSPS. The fix is a re-streamer on the Pi that translates
the camera's never-expiring RTSPS feed into HLS — which the camera
tile renders natively.

```bash
sudo bash ~/filebrowser-NC/pi-setup/scripts/install-go2rtc
# Paste the camera's RTSP/RTSPS URL when prompted.
```

Then in Settings → Machine, set Camera URL to:

```
http://<pi-host>:1984/api/stream.m3u8?src=mill-cam
```

Camera type = HLS (or Auto — the .m3u8 suffix routes correctly).

The script is idempotent — re-run after editing `/etc/go2rtc/go2rtc.yaml`
to refresh the systemd unit. To add more cameras, edit the YAML
directly and `sudo systemctl restart go2rtc`.

go2rtc's own web UI lives at `http://<pi-host>:1984` for quick
diagnostics.

## Files installed

| Path | Purpose |
|---|---|
| `/etc/cnc-pi.conf` | All knobs in one place. Source of truth for re-runs. |
| `/etc/systemd/system/cncd.service` | Headless CNC daemon (SERVICE=cncd, the default) |
| `/usr/local/bin/cncd` | The cncd binary itself — not in the repo checkout, unlike filebrowser |
| `/etc/cncd/config.json` | Machines, displays, Discord, `machineToken` — see "The three files that hold secrets" above |
| `/etc/cncd/env` | `CNCD_MACHINE_TOKEN` + any shared secrets, sourced by `cncd.service` |
| `/etc/udev/rules.d/99-cnc-serial.rules` | Stable `/dev/ttyHAAS` mapping for the USB→RS-232 adapter (SERVICE=cncd, ENABLE_SERIAL=y) |
| `/etc/systemd/system/filebrowser.service` | Web file manager (SERVICE=filebrowser) |
| `/etc/systemd/system/cnc-autodeploy.service` + `.timer` | Unattended deploy — builds cncd or filebrowser depending on `SERVICE` |
| `/etc/systemd/system/cnc-usb-mass-storage.service` | Loads `g_mass_storage` at boot |
| `/etc/systemd/system/cnc-usb-watcher.service` | The bidirectional sync watcher |
| `/usr/local/bin/cnc-usb-watcher` | The watcher script itself |
| `/usr/local/bin/cnc-status` / `cnc-rebuild` / `cnc-autodeploy` | Diagnostic dump, manual deploy, and unattended-deploy scripts — all `SERVICE`-aware |
| `/var/lib/cnc-usb-watcher/last_sync_listing` | Snapshot of the image's contents at the last successful sync. Lets the watcher distinguish controller-created files from host-deleted ones. |
| `/boot/firmware/config.txt` | `dtoverlay=dwc2` line appended (backup written) |
| `/boot/firmware/cmdline.txt` | `modules-load=dwc2` appended (backup written) |
