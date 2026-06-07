# eterminal-display

PlatformIO firmware for the **Seeed reTerminal E1001** (XIAO ESP32-S3 +
7.5" 800×480 monochrome e-paper). Reads the reconciled per-machine
tool list from filebrowser-NC and renders it as a 2×10 pocket map plus
paged library tables.

Fully local — no cloud, no SenseCraft, no TRMNL. WiFi to the LAN, HTTP
to the filebrowser-NC host, done.

## Hardware

- **MCU**: XIAO ESP32-S3 (USB-C, native USB, 8 MB flash).
- **Panel**: 7.5" 800×480 mono e-paper (Waveshare/GoodDisplay controller),
  driven via SPI + the reTerminal E1001 baseboard's FFC ribbon.
- **SD slot**: on the baseboard, wired to `D5` chip-select.
- **Button**: single user button on `D6` (active LOW, internal pull-up).

Pin numbers come from Seeed's official reTerminal E1001 example for
`XIAO_ESP32S3` with `EPD_SELECT = 0` (mono panel option). Do not guess
pins — if you migrate to the tri-color panel option, the pin map
changes; consult the Seeed cookbook.

## First flash (USB-C, one time)

1. Install PlatformIO (VS Code extension or the CLI):

   ```bash
   pipx install platformio
   ```

2. Build + flash:

   ```bash
   pio run -e e1001 -t upload
   pio device monitor      # 115200 baud
   ```

3. Prepare the SD card. Copy `data/config.example.json` to `/config.json`
   at the root of the SD card and fill in:

   ```json
   {
     "wifi_ssid": "shop-wifi",
     "wifi_pass": "<your wifi password>",
     "base_url": "http://192.168.20.10",
     "display_id": "<the ID generated when you created the display in settings>",
     "token": "<optional bearer token>"
   }
   ```

4. Insert the SD card, power-cycle. The screen renders the pocket map
   on success; an instructive splash on SD / WiFi failure.

## Registering a display in filebrowser-NC

1. Open `/settings/displays` in the web UI (admin only).
2. Click **Add display**.
3. Pick the target machine from the dropdown.
4. (Optional) Enter a token — when set, the device must present it in
   `Authorization: Bearer <token>` or `?token=`. Empty = LAN-permissive.
5. Click **Save all**. The server mints a stable `id` — paste that into
   the SD card's `config.json`.
6. The Resolution / Pocket grid / Library page size / Fields fields all
   have sensible defaults for the E1001 (800×480, 2×10, 20 rows, the
   standard column order). Override per-display if you want a different
   layout.

## Updating the firmware

You should never need the USB cable after the first flash.

### Over WiFi (ArduinoOTA)

The firmware advertises itself as `eterminal-<display_id>.local` on the
LAN. From the same PlatformIO project:

```bash
pio run -e e1001 -t upload --upload-port eterminal-<display_id>.local
```

When the display has a `token` set, OTA uses it as the password.

### Via SD card (no LAN required)

Drop the new `firmware.bin` (the file PlatformIO writes to
`.pio/build/e1001/firmware.bin`) at the root of the SD card. On the
next boot the firmware will:

1. Detect the file.
2. Stream it through `Update.write()`.
3. Rename it to `firmware.bin.applied` so the same `.bin` isn't flashed
   twice.
4. Reboot.

If the SD-mounted `.bin` is invalid the firmware logs the reason over
USB CDC and continues running the previous version.

## Display surfaces

- **Page 0 — Pocket map.** A `pocket_cols × pocket_rows` grid (default
  2×10 for the 20-pocket Haas carousel). Each cell shows pocket #,
  tool #, short description, diameter, length. Empty pockets render as
  "empty".

- **Pages 1..N — Library table.** `library_page_size` rows per page,
  with columns from `fields` (default: pocket, tool_number, description,
  diameter, length, wear). All paging is local; no refetch.

- **Status bar (top of every page).** Machine name + connected /
  disconnected (when online) or `SERVER OFFLINE - cached` (when serving
  from `/cache.json`). Plus the payload's `last_updated_display`
  string (formatted server-side; the device has no RTC) and `page x/N`.

### Pressing the button

Advances to the next page. Wraps from the last library page back to the
pocket map. Page changes never refetch — they render the cached
payload, so paging works even when the server is unreachable.

## Power notes

USB-C is the expected powered path. The firmware uses `poll_powered_s`
(default 60 s; configured per display in the UI) for the refetch
cadence. Battery operation is supported by the hardware but VBUS
detection on the XIAO ESP32-S3 is not trivially exposed — the firmware
keeps the powered cadence regardless until that's straightforward.

If you want longer battery life, lower `poll_powered_s` is not the
answer (the e-paper refresh dominates current draw, not the WiFi poll).
A future commit can add proper deep-sleep + EXT0 wake on the button.

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| Boot splash says "config.json missing" | SD not mounted, or `/config.json` malformed. Check the serial monitor at 115200. |
| "SERVER OFFLINE - cached" stays on | LAN reachable but base_url wrong, or the display_id doesn't match a registered display. |
| Pocket map all "empty" | The target machine hasn't been polled yet, or its `toolSlots` configuration is 0. Run the tool-table read once from the dashboard. |
| OTA upload rejected | `token` is set on the display but you didn't pass it as `--auth=<token>` to `pio run`. |

## File layout

```
eterminal-display/
├── platformio.ini
├── README.md
├── data/
│   └── config.example.json   ← reference for the SD /config.json
└── src/
    └── main.cpp
```
