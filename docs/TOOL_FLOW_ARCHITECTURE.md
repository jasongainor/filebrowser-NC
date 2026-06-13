# filebrowser-NC — Tool Flow & Display Architecture

> Purpose: a single reference for how the whole CNC stack fits together —
> the Haas machine connection, the tool data model (Fusion library ↔ live
> controller tool table ↔ reconciled view), the program-streaming pipeline,
> and the e-paper display — so we can design **tool-flow polish** and
> **notifications for bad / mismatched tools vs. the running program**.
>
> Pair this with the toolpath/cutconfig prompt: that describes how cut
> configs are authored; this describes how tools get from the library to the
> machine, how the app reconciles what the machine actually reports, and the
> exact seams where mismatch detection would plug in.
>
> All paths are relative to the repo root. `file.go:NN` references are
> navigational hints — re-read the file, line numbers drift.

---

## 0. TL;DR — the data journey

```
Fusion 360 tool library (operator export, JSON)
        │  PUT /api/cnc/tool-library
        ▼
ToolLibrary (byNum: PostProcess.Number → FusionTool)         cnc/tool_library.go
        │
        │                   Haas TM-2P  ──RS-232──►  Waveshare RS-232↔TCP bridge
        │                                              (host:port, e.g. 192.168.20.200:4196)
        │                                                       │ TCP
        │   POST /api/cnc/tool-table  ──Q600 macro reads──►  Streamer            cnc/streamer.go
        │        (length/dia geom+wear per slot, 2-pass)          │
        ▼                                                          ▼
   describeSlot()  ◄────────  BuildToolList(table, library)  ◄── ToolTable dump (newest .json on disk)
        │                          cnc/toollist.go                cnc/tooltable.go
        ▼
   ToolList { machine, pockets[], library[] }
        │
        ├── GET /api/machines/{id}/toollist     (dashboard)        http/cnc_toollist.go
        └── GET /api/displays/{id}              (e-paper firmware) http/cnc_displays.go
                    │
                    ▼
            reTerminal E1001 e-paper            eterminal-display/src/main.cpp
```

Two independent telemetry/transport facts to hold onto:

1. **One TCP client to the bridge at a time.** The `Streamer` owns the
   socket; tool-table reads, live polling, and program streaming all
   serialize through it (`queryMu`, 150 ms min spacing). You cannot read the
   tool table *while a program is streaming*.
2. **The reconciled view is computed on demand** from (a) the *newest*
   persisted tool-table dump and (b) the current Fusion library. Nothing is
   cached server-side; every `/toollist` / `/displays` call rebuilds it.

---

## 1. The e-paper display (reTerminal E1001)

Firmware: `eterminal-display/src/main.cpp` (PlatformIO, `env:e1001`, XIAO
ESP32-S3 + 7.5" 800×480 mono panel via GxEPD2 `GxEPD2_750_T7`).

### 1.1 Hardware / pin map (verified against Seeed V1.2 schematic + Zephyr DTS)

| Signal | GPIO | Notes |
|---|---|---|
| SD power-enable (`SD_EN`) | **16** | drives a TPS22916 load switch; **must be HIGH before `SD.begin()`** or the slot is dead |
| SD chip-select | 14 | |
| SD card-detect | 15 | active-LOW (unused) |
| SPI bus (shared with EPD) | SCK 7 / MISO 8 / MOSI 9 | XIAO default; the one thing the original firmware had right |
| EPD CS / DC / RST / BUSY | 10 / 11 / 12 / 13 | BUSY active-LOW |
| Buttons KEY0 / KEY1 / KEY2 | 3 / 4 / 5 | active-LOW, board pull-ups. GPIO3 is a strapping pin (don't hold at reset) |

Serial: there is **no USB-CDC / `usbmodem`** on this board — the baseboard
USB-C is a CH340 UART bridge wired to UART0. Firmware must build with
`ARDUINO_USB_CDC_ON_BOOT=0` so `Serial` routes to UART0; monitor at
`/dev/cu.usbserial-*` @ 115200. `upload_speed = 460800`.

### 1.2 Boot / runtime flow (`setup()` / `loop()`)

1. `Serial.begin(115200)`; drive `SD_EN` HIGH; `SPI.begin()`; `display.init()`.
2. `loadConfigFromSD()` — mount SD (`SD.begin(CS, SPI, 4 MHz)`), read
   `/config.json`, parse `wifi_ssid`, `wifi_pass`, `base_url`, `display_id`,
   `token`. Requires `wifi_ssid`, `base_url`, `display_id`. On failure →
   render a splash and idle forever.
3. `maybeFlashFromSD()` — SD-card firmware self-update if `/firmware.bin`.
4. `connectWiFi()` — STA join (with a diagnostic scan that logs all visible
   SSIDs so a typo is obvious), then `setupOTA()`.
5. `fetchPayload()` — `GET {base_url}/api/displays/{display_id}` (optional
   `Authorization: Bearer {token}`); cache last good payload to `/cache.json`.
6. `renderAll()`; then `loop()` polls every `pollIntervalPoweredS`, handles
   buttons, and emits a 5 s heartbeat to serial.

`base_url` **must include the backend port** (`http://HOST:8080`) — without
it the firmware hits port 80 and the table renders OFFLINE.

### 1.3 Display contract: `GET /api/displays/{id}`

Returns `{ config, data }` (`http/cnc_displays.go`). `config` is the Display
record (resolution, `pocketGrid`, `libraryPageSize`, `fields`,
`pollIntervalPoweredS/BatteryS`, `units`, token redacted). `data` is the
**ToolList** payload (§4.3) — the same shape the dashboard reads. This is the
display-agnostic contract: browser kiosk, e-paper, and any future surface all
parse the same JSON.

### 1.4 Rendering & pagination

- **Page 0 — pocket map** (`renderPocketMap`): the `pocketGrid` (2×10) drawn
  as bordered cells. Each populated cell shows two rows: `P# T#` + dia/len
  right-aligned (header), and the description beneath. Empty pockets show
  `(empty)`. Shows *all fields per pocket* but is space-constrained.
- **Pages 1..N — library table** (`renderLibraryPage`): one page per
  `libraryPageSize` (10) library rows, drawn as a real grid (header row +
  column separators + row lines). Columns from `config.fields`
  (`pocket | tool_number | description | diameter | length | wear`),
  description column flexes, numeric columns fixed-width. Missing values
  render as ASCII `-` (the font has no em-dash glyph).
- **Status bar** (`renderStatusBar`): machine name + connected/offline,
  `last_updated`, and `page x/N`.
- Total pages = `1 + ceil(library.size / libraryPageSize)`.
- **Buttons** (`handleButtons`, active-LOW, 200 ms debounce): left = previous
  page, right = next page (wraparound), Refresh = force a server re-fetch.

### 1.5 Known display gotchas (these bit us; capture for the polish design)

- **`pocketGrid` rows × cols** are fixed; the pocket map can only show
  `pocketCount` cells. A tool in slot 24 never appears on page 0 if the grid
  is 20 (see also §7 slot-count limits — the deeper cause is server-side).
- **Wear shows `0.000`** for every tool when the controller's `length_wear`
  is 0 (it usually is unless the operator sets length-wear offsets). The Wear
  column is **length wear**, not diameter wear. Not a render bug — it's the
  data (see §4.2/§7).
- **Proposed:** a Display setting to choose the page-0 layout (cell map vs.
  always-on dia/len/wear table) and an optional portrait (−90°) rotation —
  see §8.4.

---

## 2. Backend: machine connection & live state

`cnc/registry.go`, `cnc/streamer.go`, `cnc/state.go`, `cnc/qcode.go`,
`settings/settings.go`.

### 2.1 Registry — coordinator

`Registry` (`cnc/registry.go`) holds, per machine ID: a `Streamer`, an
`Aggregator`, the `QueueStore`, the `Notifier`, and the `LibraryStore`.
`Refresh()` diffs `settings.Cnc.Machines` and adds/removes machines.
`Streamer(id)` / `Aggregator(id)` look up by ID; empty ID falls back to
`DefaultMachineID()` (first machine).

### 2.2 Streamer — the one socket to the machine

`Streamer` (`cnc/streamer.go`) owns the TCP connection to the Waveshare
**RS-232↔TCP bridge** at `Machine.Host:Machine.Port` (port `0 → 4196`).
Transport is raw TCP carrying Haas RS-232 framing. Invariants:

- **Single job** at a time (`s.job != nil` → `ErrJobAlreadyRunning`).
- **`queryMu`** serializes all Q-code queries; **`minQuerySpacing` (150 ms)**
  throttles them (RS-232 side is the bottleneck).
- `lastError` (a.k.a. `HaasLastError`) persists across jobs; cleared on a
  successful `Start()`.
- Crash recovery: `Start()` writes an `activeJobMarker`; a marker found at
  boot means a job was interrupted (Z-15 recovery gate).

### 2.3 Q-codes — the telemetry protocol (`cnc/qcode.go`)

Wire: `?Q<code>[ <var>]\r\n` → `…\x02<payload>\x17…`. `Query(ctx, qCode,
macroVar)` returns a `QueryResult{ Value, Parsed, OK, … }`. Macro reads use
**Q600 + a macro var** (e.g. `#3030` current block, `#3027` actual RPM,
`#5021..5023` machine pos). Tool-table offsets are Q600 reads of macro bases
**2001 (len geom) / 2201 (len wear) / 2401 (dia geom) / 2601 (dia wear)** +
`(slot-1)` (`cnc/tooltable.go`). Responses are shape-validated to reject
cross-talk frames.

### 2.4 Aggregator — background polling & "connected"

`Aggregator` (`cnc/state.go`) runs ~16 goroutines, one per metric
(`mode` Q104, `tool` Q201, `status_combined` Q500, `current_block`,
positions, spindle, offsets), on staggered intervals (1.5–30 s), caching
results in a `metrics` map exposed by `Snapshot()` (`/api/cnc/state`).

- Polls **only while `IsAwake()`** (a wake window, default 5 min, extended by
  `/state`, `/check`, `/start`) — saves bridge traffic when idle.
- **Never polls during a stream** (`streamer.IsRunning()` → mark stale, skip)
  — the streaming socket carries G-code, interleaved Q-codes would corrupt.
- "connected" in the ToolList = aggregator `IsAwake()` AND no
  `HaasLastError` on the streamer (`http/cnc_toollist.go`
  `buildMachineToolList`).

### 2.5 Machine settings (`settings/settings.go`)

`Machine{ ID, Name, Brand, Host, Port, ToolSlots, RequirePreflight,
AxesEnabled, PositionToleranceIn, DPRNTCapture, AutoSendEnabled,
NoProbeSlots, … }`. `EffectiveToolSlots()` clamps `ToolSlots` to [1,200]
(default 30). **This value is both the magazine size AND the default
slot-count for a tool-table read** — see §7.

---

## 3. Backend: the tool data model

### 3.1 ToolLibrary — what the operator catalogued (`cnc/tool_library.go`)

Sourced from a **Fusion 360 tool-library export** (JSON), uploaded via
`PUT /api/cnc/tool-library` and stored at
`$XDG_CONFIG_HOME/filebrowser-NC/tool-library.json` (atomic write). Parsed
into `ToolLibrary{ raw, byNum }` where **`byNum` is keyed by
`FusionTool.PostProcess.Number`** (the pocket/tool number on the machine,
1–200; entries with `Number ≤ 0` or holder-only are skipped; first-write-wins
on dup numbers). `Lookup(n)` returns the `FusionTool` for number `n`.

`FusionTool` carries: `Description`, `Vendor`, `Type`, `Geometry{ DC
(cutting dia), NOF (flutes), OAL, RE (corner radius), … }`, `Holder` (profile
segments for the magazine render), `PostProcess{ Number, DiameterOffset,
LengthOffset, … }`, vendor `ProductID/Link`, and raw speeds/feeds.

This is the **catalog / intent** side: descriptions, geometry, vendor — but
*not* live measured offsets.

### 3.2 ToolTable — what the controller actually reports (`cnc/tooltable.go`)

A `ToolTable{ MachineID, ReadAt, SlotsRequested, SlotsRead, Slots[], Source
}` produced by `ReadToolTable(ctx, slots)` over the bridge. Each
`ToolTableSlot{ Slot, LengthGeom, LengthWear, DiameterGeom, DiameterWear,
EffectiveDiameter, EffectiveLength, Empty, Errors, ManuallyEdited, EditedAt
}` uses **pointers** so `nil` = "not read", `0` = "controller returned zero
(unset offset)".

Read strategy (two-pass, to skip empty pockets cheaply):
1. **Pass 1:** read `length_geom` + `diameter_geom` for every slot. A slot is
   "populated" if either is non-zero.
2. **Pass 2:** read `length_wear` + `diameter_wear` for populated slots only.
3. Compute `Effective*` = geom + wear when both present.
4. **On cancel/timeout the partial table is still returned**, and every
   unreached slot gets its `Errors` stamped `"cancelled: …"`. *(This is why a
   read that's dismissed/drops at slot 5 leaves slots 5–N flagged offline —
   exactly the stale-dump symptom we debugged.)*

Persisted to `<user-scope>/cnc-tool-tables/<machine-id>/<RFC3339>.json`;
**newest file wins** on read (`http/cnc.go` `persistToolTable` /
`newestJSONIn`). Manual offset edits (`POST /api/cnc/tool-table/edit`) write a
new dump with `Source="edit"`, `ManuallyEdited=true` on the touched slot, and
clear that slot's errors (dashboard-local; **no G10 write-back to the
controller yet**).

### 3.3 Reconciliation — `BuildToolList()` (`cnc/toollist.go`)

`BuildToolList(machineID, name, units, connected, pocketCount, librarySize,
table, library)` joins the two sides into the wire payload. Index the table
by slot (`byNum[s.Slot] = s`), then:

**Pockets pass** — exactly `pocketCount` rows (`for p := 1..pocketCount`):
```
row = { Pocket: p }
if byNum[p] exists AND !isEmptyOrOffline(s):
    row.ToolNumber  = p                       // *** CAROUSEL: pocket N == tool N ***
    row.Description = describeSlot(s, library, p)
    row.Diameter    = nonZeroOrNil(s.EffectiveDiameter)
    row.Length      = nonZeroOrNil(s.EffectiveLength)
    row.Wear        = nonZeroOrNil(s.LengthWear)   // *** length wear only ***
```
`isEmptyOrOffline(s)` = `s.Empty || len(s.Errors)>0 || (all four offsets nil)`
→ such a pocket reports `tool_number: null`.

**Library pass** — `for n := 1..librarySize(200)`, include `byNum[n]` only if
it exists; **drop all-zero rows** unless they have a library description or
are offline. `Offline = len(s.Errors) > 0`. Sorted by tool number.

`describeSlot()` priority: (1) library `Description`; (2) synthesized
`Vendor · Type · D=<DC> · <NOF> flutes`; (3) empty. `nonZeroOrNil`
(`cloneNonZeroPtr`) collapses `0 → nil` so an unset `0.0000` doesn't render as
a real measurement.

### 3.4 ToolList payload shape (the §1.3 `data`)

```
ToolList {
  machine: { id, name, connected }
  last_updated, last_updated_display, units
  pockets:  [ { pocket, tool_number|null, description, diameter?, length?, wear? } ]  // length == pocketCount
  library:  [ { tool_number, pocket, description, diameter?, length?, wear?, offline } ]
}
```

---

## 4. Backend: program loading & execution

`cnc/queue.go`, `http/cnc_autosend.go`, `cnc/streamer.go`, `cnc/registry.go`.

- **Queue:** `QueueStore.Add` parses the program's O-number
  (`readONumber`) into `OnumberHint`. One row per machine can be in-flight.
- **Send/stream:** `Streamer.Start(absPath, displayPath, method)` →
  `run()` opens the file and writes it **line-by-line** over the socket,
  bumping an atomic `lineCurrent`, emitting `Event{Type:"line"}`, and
  scavenging DPRNT between writes. **No tool parsing or validation happens in
  this loop.**
- **Auto-attach:** the aggregator's Q500 `status_combined` carries the
  controller's current program; `extractProgram()` + `PromoteByONumber()` /
  `FindByONumber()` match it to a queued file and `AttachAuto()` it, so the
  dashboard follows along even when the program was started at the control.
- **What the app knows:** the file it *sent* (`Status.FilePath`), the program
  the controller *reports* running (Q500), the *current spindle tool* (Q201
  `#3027`/`tool`), and the *current block* (Q600 `#3030`).

---

## 5. Tool-vs-program validation — today

### 5.1 Preflight (static, pre-send) — `cnc/preflight.go`

`BuildPreflight(absPath, …, machineID, table, currentSpindleTool)` parses the
**source file on disk** (not the running program) for tool usage:
- Tool-comment headers (`(T5 D=0.5 CR=0.06 …)`) → `ExpectedDiameter`,
  `ExpectedCornerRadius`.
- Active `T\d{1,3}` calls → `ReferenceCount`, `startingTool`.
- Cutter comp `G41/G42` → flags diameter-critical tools.

Then classifies each tool against the **latest tool-table dump**:
- not in table → `missing`; slot has errors → `offline`; read-but-all-zero →
  `empty` (escalated to `warn` under cutter comp);
- `UsesCutterComp` with no/zero diameter → `warn`; `|expected − actual|` dia
  delta > tolerance (0.005") → `warn`;
- `StartingTool != CurrentSpindleTool` → `SpindleSwap` (UI flag, non-blocking).

Gates: when `Machine.RequirePreflight`, `POST /api/cnc/start` returns **409**
if the table is missing or any tool is missing/empty (`http/cnc.go`).
Auto-send (`http/cnc_autosend.go`) blocks on any warn/missing/empty/offline or
a pending spindle swap.

### 5.2 Live feedback / events / notifications

- **DPRNT** (`cnc/dprnt.go`): captures `DPRNT[…]` macro output during a run to
  a sidecar log + `Event{Type:"dprnt"}`. **Does not parse T-codes / M06.**
- **Chapters** (`cnc/chapters.go`): operation-header TOC for UI navigation;
  not tool-related.
- **Events** (`cnc/events.go`): `line | status | metric | log | queue |
  dprnt`. **Notifications** (`cnc/notify.go`, triggered in `registry.go`):
  job started/ended, attach changes, machine failures (`HaasLastError`),
  streamer errors. **There is no tool-mismatch event or notification.**

---

## 6. Known gaps & assumptions (design inputs)

These are the seams the tool-flow polish must address. Several directly
explain display behavior the operator already noticed.

1. **Carousel assumption — pocket N == tool N** (`cnc/toollist.go`).
   Hardcoded everywhere. A side-mount magazine (tool 24 living in pocket 5)
   cannot be represented; the pocket pass literally sets `ToolNumber = p`.
   *(This is why "mismatched pockets vs tools" experiments don't show as
   expected.)* TODO: `project_filebrowser_nc_side_mount_mapping_todo.md`.

2. **Slot-count bounds visibility.** `pocketCount = EffectiveToolSlots`
   bounds the `pockets` array, and the **library only includes slots that
   were actually read**. A tool with offsets in slot 24 won't appear if the
   machine's `ToolSlots` is 20 (read defaults to 20). To surface it: raise
   `ToolSlots`, or read with `?slots=N`.

3. **Wear semantics.** The pocket/library `wear` field maps to
   **`LengthWear` only**, and `0` collapses to "no measurement" → renders
   `0.000`. Diameter wear (e.g. slot 8's −0.0025) is folded into
   `EffectiveDiameter` but never shown as "wear". An operator who never sets
   length-wear offsets sees `0.000` across the board (correct, but
   surprising).

4. **No runtime tool validation.** Preflight is static and pre-send. Nothing
   parses T-codes/M06 *as the program streams*, nor compares the live spindle
   tool (Q201) against what the program is about to call. No tool-change event
   exists; job history records no tool sequence.

5. **No tool-mismatch notification path.** Notifications cover job/attach/
   failure only.

6. **Manual edits are dashboard-local.** No G10 write-back; the controller
   still holds the original offsets until the next real read.

---

## 7. Design targets — polishing tool flow + mismatch notifications

### 7.1 What "mismatch" can mean (taxonomy to design against)

- **Missing/empty/offline** tool the program needs (preflight already, static).
- **Diameter/length mismatch** between program intent (CAM header / cutconfig)
  and measured offset (preflight does diameter pre-send; not at runtime).
- **Wrong tool loaded** — program calls `T7` but spindle has `T3` (Q201) —
  *not checked today*.
- **Carousel/pocket mismatch** — tool physically in a different pocket than
  the model assumes — *not representable today* (carousel).
- **Stale tool table** — offsets read long ago, tool re-ground since — *no
  freshness check beyond `last_updated`*.

### 7.2 Integration seams (smallest → largest)

- **Seam A — Streamer line parser** (`cnc/streamer.go` run loop): regex each
  outgoing line for `T\d+` / `M06`; emit a new `Event{Type:"tool_change"}`.
  Cheapest; gives a real-time tool-sequence signal.
- **Seam B — Registry watcher** (`cnc/registry.go` event handler): on a Q201
  change mid-run, compare against the preflight tool set / expected next tool
  and fire a `NotifyCategory…` "tool mismatch". Reuses existing notify plumbing.
- **Seam C — Aggregator composite metric** (`cnc/state.go`): a metric that
  reads Q500 (program) + Q201 (tool) and precomputes a "matches expectation"
  boolean in `Parsed`, consumable by UI and notifier.
- **Seam D — Preflight tool sequence** (`cnc/preflight.go`): emit an ordered
  `[(line, tool, expected_dia, cutter_comp)]` alongside the per-tool summary,
  so the streamer can validate each change against intent at runtime.

### 7.3 Phasing

1. **MVP:** `tool_change` event (Seam A) + mismatch notification (Seam B)
   comparing live Q201 against preflight's expected tools. Surfaces "program
   wants T7, spindle has T3" on the dashboard, Discord, and the e-paper.
2. **Confirmation UX:** look-ahead in the streamer for an upcoming `M06`,
   emit `tool_change_pending`, optional operator-ack gate.
3. **Full validation:** preflight tool sequence (Seam D) + per-change offset
   re-check against the cutconfig's intended diameter/length, with a freshness
   policy on the tool-table dump.

### 7.4 Display-side enhancements (operator-requested)

- **Layout setting in Display settings.** Add a `Display` config field (e.g.
  `pocketView: "cells" | "table"` and/or `rotate: 0 | 90`) returned in
  `config`. Firmware honors it in `renderAll`. Rationale: the operator prefers
  **dia/len/wear visible at all times**, which the page-1+ *table* layout
  gives but the page-0 *cell map* sacrifices for density. A setting lets each
  display pick density-vs-always-on-columns.
- **Portrait (−90°) mode** for evaluating the narrower-header / taller-rows
  trade. `display.setRotation(3)` + swap logical width/height (480×800) so the
  existing layout math reflows.
- Wear column should clarify it's length wear (or optionally show diameter
  wear / a combined indicator) once §6.3 is settled.

---

## 8. File:line index (navigational)

| Area | Files |
|---|---|
| Display firmware | `eterminal-display/src/main.cpp`, `eterminal-display/platformio.ini` |
| Machine connection | `cnc/streamer.go`, `cnc/registry.go`, `cnc/state.go`, `cnc/qcode.go`, `cnc/recovery.go` |
| Tool library | `cnc/tool_library.go`, `http/cnc_tool_library.go` |
| Tool table (read) | `cnc/tooltable.go`, `http/cnc.go` (`ReadToolTable` handler, `persistToolTable`, `newestJSONIn`) |
| Reconciliation | `cnc/toollist.go`, `http/cnc_toollist.go` |
| Display/kiosk endpoint | `http/cnc_displays.go` |
| Manual edits | `http/cnc_tool_table_edit.go` |
| Program send/stream | `cnc/queue.go`, `cnc/streamer.go`, `http/cnc_autosend.go` |
| Preflight (validation) | `cnc/preflight.go`, `http/cnc.go` (preflight + start handlers) |
| Live telemetry / logs | `cnc/dprnt.go`, `cnc/chapters.go`, `cnc/events.go`, `cnc/notify.go`, `cnc/job_history.go` |
| Machine settings | `settings/settings.go` |

---

## 9. Where the cutconfig / toolpath prompt plugs in

The toolpath/cutconfig prompt defines **intent** — for a given operation,
which tool, what diameter/corner-radius, speeds/feeds. This stack defines
**reality** — what the controller actually has loaded and measured. The polish
work is the bridge between them:

- Map a cutconfig's tool intent → expected `(tool_number, diameter, length,
  cutter_comp)` (overlaps `ToolUsage` in preflight, today sourced from CAM
  comments — a cutconfig could supply it authoritatively instead).
- Validate that intent against the live tool table + spindle tool, **before
  send (preflight, exists)** and **during the run (Seams A–D, to build)**.
- Notify on any §7.1 mismatch class, on the dashboard + Discord + e-paper.

Open questions for the design pass: do we trust the cutconfig or the CAM
header as the source of expected geometry? do we add per-tool offset freshness
(re-read before a critical op)? and do we finally break the carousel
assumption to model true pocket↔tool mapping (prerequisite for honest mismatch
detection on side-mount machines)?
