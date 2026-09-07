# Direct serial transport (bypassing the Waveshare bridge)

Status: implemented in `cnc/transport.go`, `cnc/serial_transport.go`,
`cnc/flowcontrol.go`. Not verified against a real Haas TM-2P — everything
here has been exercised against a Linux pty pair
(`cnc/serial_transport_test.go`, `cnc/flowcontrol_test.go`), which gets the
real termios/ioctl code paths right but cannot exercise real RS-232
electrical behavior, real baud-rate timing, or modem-status lines.

## Why this exists

Every machine so far talks to a Raspberry Pi through a Waveshare
RS-232↔TCP bridge. That works for polling and MEM-tab uploads, but it
structurally cannot carry DNC drip-feed with flow control: a TCP socket
has no side channel for an in-band XON/XOFF byte to pre-empt a write
already queued in the kernel's send buffer, and the Waveshare doesn't
expose the RTS/CTS lines to the Ethernet side either. See
`docs/PROGRAM_DELIVERY_AND_LIBRARY_SYNC_TODO.md` section B for the longer
version of this problem.

The fix: let the Pi own the RS-232 line directly. A USB→RS-232 adapter on
the Pi, wired straight to the Haas' RS-232 header, gets Setting 14
(handshake) for real — flow control is just bytes (or modem-status bits)
on a serial device Go can read with normal deadlines, no bridge in
between.

This is additive. A Machine with no `serial.device` set behaves exactly
as before — TCP to `host:port`. Nothing about the Waveshare path changed.

## Wiring

```
Raspberry Pi                         Haas TM-2P (RS-232, DB-25)
┌──────────────┐                     ┌───────────────────────┐
│ USB-A port   │──USB→RS-232 cable──│ DB-25 serial connector │
│ (ttyUSB0/    │   (null-modem       │ (rear of control)      │
│  by-id path) │    wiring: TX↔RX,   │                        │
│              │    RTS↔CTS if used) │                        │
└──────────────┘                     └───────────────────────┘
```

- Most USB→RS-232 adapters (FTDI, Prolific) show up as `/dev/ttyUSB0`.
  Prefer the stable path under `/dev/serial/by-id/…` if more than one
  serial device can ever be plugged into the Pi — `ttyUSB0` numbering is
  not guaranteed to survive a reboot or a replugged cable.
- The cable must be wired **null-modem** (TX↔RX crossed), matching
  whatever cable the shop already uses for MEM-tab RS-232 transfers to
  the Waveshare — this is the same electrical connection, just with a Pi
  USB-serial adapter on the far end instead of the Waveshare's RS-232
  side.
- For `flowControl: "rtscts"`, RTS/CTS must actually be wired
  (pin 4↔5 crossed on a DB-25/DB-9 null-modem cable). A lot of shop RS-232
  cables only carry TX/RX/GND — check before setting this.

## Haas settings that must match

All of these are in `cnc/haas_codes.go`'s curated Settings catalog
(`/api/cnc/codes/setting/<n>` surfaces the same text in the UI):

| Setting | Name                        | Must be                          | `MachineSerial` field |
|---------|-----------------------------|-----------------------------------|------------------------|
| 11      | Baud Rate Selection         | matches `baud`                    | `baud`                 |
| 12      | Parity Selection             | matches `parity`                  | `parity`                |
| 13      | Stop Bits                   | matches `stopBits`                | `stopBits`              |
| 14      | Synchronization (Handshake) | `XON/XOFF` (or `RTS/CTS` if wired)| `flowControl`           |
| 37      | RS-232 Data Bits             | matches `dataBits`                | `dataBits`              |
| 143     | Machine Data Collect         | **ON**                            | n/a — required for Q-codes regardless of transport |
| 55      | Enable DNC from Device       | ON, and the pendant switched to DNC mode before Cycle Start | n/a — operator step, same as the TCP path |

Setting 143 and Setting 55 are unrelated to which transport carries the
bytes — they're required whether you're on the Waveshare bridge or
direct serial. They're listed here because "DNC drip-feed doesn't work"
after switching to serial is more often a missed Setting 55 than a wiring
problem.

## Configuration — `Machine.serial`

Add a `serial` block to the Machine entry in Settings → Machine (JSON
shape shown; the field lives in `settings.MachineSerial` /
`settings.Machine.Serial`):

```json
{
  "id": "tm2p-1",
  "name": "TM-2P",
  "brand": "haas",
  "host": "",
  "port": 0,
  "serial": {
    "device": "/dev/serial/by-id/usb-FTDI_USB-RS232_Cable-if00-port0",
    "baud": 9600,
    "dataBits": 7,
    "parity": "even",
    "stopBits": 1,
    "flowControl": "xonxoff"
  }
}
```

- **`device`** is the only field you must set. A non-empty `device` is
  what tells the Link to use serial instead of TCP (`cnc/transport.go`'s
  `buildTransport`) — `host`/`port` are then ignored entirely.
- Every other field defaults if left at its zero value (unset in JSON,
  `0`/`""` in Go) — see `settings.Machine.EffectiveSerial`:

  | Field         | Default    | Haas Setting it mirrors |
  |---------------|------------|--------------------------|
  | `baud`        | `9600`     | Setting 11               |
  | `dataBits`    | `7`        | Setting 37               |
  | `parity`      | `"even"`   | Setting 12               |
  | `stopBits`    | `1`        | Setting 13               |
  | `flowControl` | `"xonxoff"`| Setting 14               |

  **These are not the Haas factory defaults** (factory is 8 data bits,
  no parity, no handshake). An unattended DNC drip-feed is worthless
  without flow control, so the zero-value here resolves to 7E1 +
  XON/XOFF — the traditional pairing for DNC on Haas and older
  Fanuc-style controls — rather than silently reproducing "no handshake"
  and hanging the first time the Haas' buffer fills. If your Haas is
  genuinely left at factory serial settings, set every field explicitly.
- `parity` accepts `even` / `odd` / `none` / `mark` / `space`.
  `stopBits` accepts `1` or `2`. `flowControl` accepts `xonxoff` /
  `rtscts` / `none`. Anything else fails the dial with a descriptive
  error (surfaced the same way a bad TCP host/port is — `LinkState`'s
  `last_error`, visible on `/api/cnc/status`).

## What flow control actually does

- **`xonxoff`** (recommended, matches Setting 14's Haas-typical default):
  `cnc/flowcontrol.go`'s `flowGate` tracks XOFF (0x13) / XON (0x11) bytes
  read off the wire. `streamFile` (the DNC drip-feed line-writer in
  `cnc/streamer.go`) checks the gate before writing each line and blocks
  — up to `flowControlResumeTimeout` (60s) — if the Haas has asserted
  XOFF and not yet sent XON. If it never resumes, the job fails with
  `ErrFlowControlStalled` instead of hanging forever. Every serial read
  path (Q-code responses in `exchangeOnReader`, DPRNT capture in
  `scavengeOnce`) strips XON/XOFF bytes before the caller ever sees them,
  so a control byte the Haas happened to interleave with a Q-code
  response or a DPRNT[…] line never lands in captured text.
- **`rtscts`**: **not real hardware flow control.** The Go serial library
  this project uses (`go.bug.st/serial` v1.6.4) has no field in its
  `Mode` struct for hardware handshake, and its `Open` call unconditionally
  *disables* termios `CRTSCTS`. What `flowControl: "rtscts"` gets you
  instead is a software poll of the CTS modem-status bit
  (`serialConn.ClearToSend`, `awaitClearToSend` in `flowcontrol.go`) —
  on a properly wired null-modem cable the Haas' RTS output is crossed to
  the Pi's CTS input, so this does reflect the controller's real
  "ready to receive" signal, just checked on a timer (every 20ms) rather
  than reacted to via interrupt. Adequate for line-at-a-time drip-feed,
  untested against real hardware, and **has no automated test coverage**
  — a pty pair (all the test suite has) has no modem-status lines;
  `GetModemStatusBits` fails with "inappropriate ioctl for device" on
  one. Prefer `xonxoff` unless you specifically need RTS/CTS and have
  verified the CTS behavior at the pendant yourself.
- **`none`**: no flow control at all. Only sensible for MEM-tab sends
  (whole-program receive into memory, no drip pacing) — using this with
  `SendMethod: "dnc"` will overrun the Haas' receive buffer on anything
  but a trivially short program.

## What is still TCP-only

- **Everything not on this Machine's `serial.device`.** Per-machine, not
  global — a shop with two machines can run one over the Waveshare bridge
  and one over direct serial; each `Machine` entry picks its own
  transport independently.
- **Q-code telemetry over Ethernet.** There is no such thing on this
  control family regardless of transport (see
  `docs/PROGRAM_DELIVERY_AND_LIBRARY_SYNC_TODO.md`'s M-Net scan) — serial
  is still the only way to reach Q-codes, whether that serial link goes
  through the Waveshare or directly to the Pi.
- **The diagnostic `BridgeAddress` strings** in `cnc/probe.go`,
  `cnc/probe_life.go`, and `cnc/tooltable.go` still format as
  `host:port` and are not updated to show the serial device path for a
  serial-configured Machine. Cosmetic only — the actual connection
  (`cnc/link.go`'s `Link`) uses the correct transport regardless; only
  these diagnostic snapshots show a stale-looking address. Left alone in
  this change to keep the diff to the transport layer.
- **FNC over Net Share** (see PROGRAM_DELIVERY_AND_LIBRARY_SYNC_TODO.md
  section B) is a separate, unrelated delivery path over the network
  share, not serial at all — it remains the recommended way to get large
  programs onto the control without a receive dance on either transport.

## Testing notes

`cnc/serial_transport_test.go` and `cnc/flowcontrol_test.go` cover:

- Q-code round-trip framing over a real `serial.Open`'d pty slave
  (exercises the actual termios/ioctl path, not a mock).
- `serialConn.Read` re-shaping `go.bug.st/serial`'s `(0, nil)`
  read-timeout signal into an error the rest of the codebase's
  timeout-detection already understands.
- `newSerialTransport` config validation (bad parity / stop bits / flow
  control string) and its Haas-sensible defaults.
- XON/XOFF stripping from both the Q-code and DPRNT read paths, byte
  order preserved.
- A full `streamFile` pause: XOFF holds the line write, XON releases it,
  and every line still arrives at the peer in original order —
  including with the pause active before the send starts.
- Context-cancellation (operator Stop) winning over an in-progress
  flow-control wait, and the `ErrFlowControlStalled` timeout path.
- The pre-existing TCP path (`cnc/link_test.go`) is unmodified in
  behavior — updated only mechanically for the `net.Conn` → `Conn`
  interface rename — and still passes.

None of this touches a real Haas control or a real USB-serial adapter.
The pty gets termios/ioctl calls right; it does not validate real
baud-rate timing, real electrical RS-232 behavior, or (as noted above)
CTS/RTS modem-status lines at all.
