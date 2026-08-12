# Program delivery + tool-library identity — TODO

Two open problems, both variations on the same complaint: **the machine is
treated as a dumb serial peripheral, and the tool data is a static snapshot
an operator remembered to import.**

Nothing here is implemented. Written 2026-08-12. Pair with
`docs/TOOL_FLOW_ARCHITECTURE.md` (current data flow) and
`docs/DPRNT_RESEARCH.md` (the read-path half of the streaming socket).

---

## A. Toolpath ↔ machine ↔ screen sync

### The problem

`cnc/tool_library.go` stores **exactly one** tool library, globally, at
`$XDG_CONFIG_HOME/filebrowser-NC/tool-library.json`, indexed by
`PostProcess.Number` (the pocket number). Every reconciliation —
`BuildToolList`, `describeSlot`, the pre-flight tool check, the e-paper
display — resolves T-numbers against **that one file**, whatever it happens
to contain right now.

So the whole chain is anchored on a coincidence: that the library an
operator last uploaded is the same library the NC program in the queue was
posted against. Nothing checks it, and nothing can: **the NC file carries no
record of where its tools came from.** A `T5` in the program and a `T5` in
`tool-library.json` are matched on the integer alone.

Failure mode: post a job against library B, forget to re-upload, and the
dashboard confidently describes every pocket using library A. Pre-flight
goes green. The display shows the wrong tool descriptions. Everything looks
correct and is wrong — the worst possible shape for a safety check.

### The fix

Make the NC file self-describing, then key reconciliation off what it says.

1. **Post-processor boilerplate.** Add to the Fusion post a header block
   emitted once per program, e.g.:

   ```
   (FBNC-LIB: name=GMW-Main rev=2026-08-12T14:22Z guid=<library-guid>)
   (FBNC-TOOL: T5 guid=<tool-guid> desc="1/4 4FL Carbide EM" dia=0.2500 len=2.5000)
   (FBNC-TOOL: T7 guid=<tool-guid> ...)
   ```

   Anchor on the **per-tool GUID**, not the description or the number.
   `FusionTool.GUID` already exists in the model (`cnc/tool_library.go`) and
   is stable across renames and pocket re-assignment — it is the only
   identity Fusion actually guarantees. A library-level name/rev is a
   convenience for humans reading the log; the GUIDs are what match.

   Open question: Fusion's post API exposes the tool GUID to the post
   (`tool.productId` / holder + tool properties are reachable); confirm the
   exact accessor for the library GUID before writing the post, and whether
   it survives a library copy. If there is no stable *library* GUID, drop
   that field and rely on the tool GUID set.

2. **Parse it.** Extend the NC parser that already extracts T-references for
   pre-flight (`cnc/preflight.go`) to pull the `FBNC-LIB` / `FBNC-TOOL`
   block. Keep it tolerant — an un-annotated program must still work exactly
   as it does today, just without the extra check.

3. **Store libraries plural.** `LibraryStore` becomes keyed by library
   identity instead of being a single file. Keep the current file as the
   "default / last uploaded" so nothing regresses.

4. **Reconcile against the program's library, then against the machine.**
   Three-way: what the program expects (GUIDs from the header) ↔ what the
   library says lives in each pocket ↔ what the controller actually reports
   (`cnc/tooltable.go` Q600 dump). Two new failure classes become
   detectable, and neither is today:
   - *Wrong library*: program references a tool GUID absent from any stored
     library, or present under a different pocket number.
   - *Stale machine*: pocket geometry on the controller disagrees with the
     library entry the program was posted against (beyond the existing
     drift tolerance).

5. **Surface it everywhere the tool list already goes** — the dashboard, the
   pre-flight blocker, and `GET /api/displays/{id}` so the e-paper shows
   which library the running program belongs to rather than an
   import timestamp with no provenance.

### Notes / gotchas

- `NewToolLibrary` does **first-write-wins on duplicate pocket numbers**,
  which is a silent lossy merge. Once GUIDs are the key, that ambiguity
  should become a surfaced conflict rather than a comment.
- The tool-table dump is operator-initiated only — there is no background
  dumper, so "what the machine reports" can be months stale (as of writing,
  the newest dump on the shop Pi was from 2026-06-13). Any three-way
  reconciliation must show the dump's age prominently or it will produce
  confident nonsense.

---

## B. Unattended program delivery (the "DNC doesn't work" problem)

### What actually exists today

`SendMethod` has two values (`cnc/streamer.go`): `mem` (controller receives
into program memory, operator presses Cycle Start) and `dnc` (drip-feed).
**Both are RS-232 byte streams from the Pi through the Waveshare
RS-232↔TCP bridge.** There is no other transport.

### Why "auto send" doesn't work over the current path

This is a property of the control, not of our code: **a Haas will not accept
an unsolicited program push over RS-232.** The controller has to be put into
a receiving state first, at the pendant:

- MEM path — List Program mode, then RECV RS-232.
- DNC path — Setting 55 (*Enable DNC from Device*) ON, control switched to
  DNC mode, then Cycle Start to begin consuming the drip.

So the operator walk-to-the-machine step is structural for the serial
transport. Streaming faster or reconnecting differently does not remove it.
The `cnc-persistent-link` work (2026-08-12) removed the *connection* churn;
it does not and cannot remove this.

Verify before building anything: Setting 14 (handshake) must be XON/XOFF for
drip-feed, and Setting 143 (Machine Data Collect) must be ON for the Q-code
telemetry we already depend on. Both are in the catalog
(`cnc/haas_codes.go`) but neither has been confirmed against the pendant
this year.

### SOLVED IN PRINCIPLE 2026-08-12 — FNC over Net Share

The mechanism is **FNC**, not DNC. Haas's "File Numeric Control" runs a
program directly from a *device* — USB stick, the machine's hard drive, or
**Net Share** — instead of loading it into the control's 1 MB program
memory. It is the same idea as drip-feed but over a real filesystem rather
than a serial line.

This collapses the whole problem:

- Post from Fusion straight to the share. The file is on the machine's
  `LIST PROGRAM → Net Share` tab the moment it is written. No copy step, no
  thumb drive, no RS-232 receive dance.
- The remaining operator action is *selecting the program* — which is
  unavoidable and reasonable, and is nothing like the current "set the
  control into a receive state, then start a transfer" ritual.
- It sidesteps the 1 MB memory limit for large programs.

Reference: a shop owner did exactly this on a 2009 Haas OM-2 (a pre-NGC
control, same generation as ours) using a Raspberry Pi as the file server —
"Haas FNC with Thumb Drives and Networking for pre NGC Machines via a
Raspberry Pi". The relevant details from that account:

- **The control speaks SMB1 and nothing else.** That is why a Pi running
  Samba was used at all: Windows 10 dropped SMB1 by default and he did not
  want to re-enable it there.
- Settings live under **Settings → I/O → Networking**: machine name, DHCP
  on, DNS server, domain/workgroup, **Remote Server**, **Remote Share
  Path**, and **Net Share tab enabled**.
- **Press F1 after changing any network setting.** It reads "press F1 to
  refresh settings" — without it the network processor is never reset and
  nothing takes effect. This is the step that wastes an afternoon.
- The control writes diagnostics to an `admin` folder on its hard drive,
  including an `ipconfig.txt` with its DHCP state, MAC and IP, plus a
  network-error log. Useful for debugging; **do not delete that folder.**
- FNC has a shop-forum reputation for being "flaky." He asked Haas directly
  about FNC over the network on an old control and was told it is fine, and
  reports no trouble in practice beyond thumb-drive recognition issues.
  Treat the reputation as unverified folklore, but do not bet a long
  unattended cut on it before we have run it ourselves.
- A file selected on a device shows `FNC` next to it and stays locked to
  that device; pressing SELECT PROGRAM again releases it.

### What we already have

Checked on zinc 2026-08-12 — nearly everything is in place already:

| Piece | State |
|---|---|
| Samba installed, `smbd` running | ✅ already active |
| `[cnc]` share, guest-writable, at the exact folder filebrowser serves | ✅ already exists (`pi-setup/lib/smb_share.sh`) |
| `nmbd` for NetBIOS name resolution (the "Remote Server Name") | ✅ already running |
| zinc reachable at `192.168.20.11` on the same LAN as the control | ✅ |
| **SMB1/NT1 enabled** | ❌ **the only gap** — `server min protocol` is `SMB2_02` |

Samba 4.22 on zinc still accepts `server min protocol = NT1` (verified with
`testparm` against a scratch config; the live service was not touched). So
the gap is one config line.

`SMB_LEGACY` was added to `pi-setup/setup-pi.sh` + `lib/smb_share.sh` to
emit it — **opt-in, defaulting to `n`**, because SMB1 is the protocol family
EternalBlue targeted and it should not land on every Pi this script
provisions. Enabling it is a deliberate per-shop decision.

### Remaining steps

1. Apply `SMB_LEGACY=y` on zinc and restart `smbd`. Reversible; one line.
2. At the pendant: Settings → I/O → Networking. Set Remote Server to
   `zinc` (or `192.168.20.11`), Remote Share Path to `cnc`, workgroup
   `WORKGROUP`, enable the Net Share tab — **then F1.**
3. Verify from the control: `LIST PROGRAM → Net Share` should list the
   contents of `/home/admin/Desktop/cncFiles`.
4. Point the Fusion post output directly at the share so posting *is*
   delivery.
5. Decide the security posture. SMB1 guest-writable is fine on an isolated
   shop LAN and is what the share already is; it is the wrong answer if this
   segment routes anywhere untrusted. Worth confirming the VLAN before
   leaving it on.

Note this does **not** obsolete the RS-232 bridge — Q-code telemetry, the
tool table, and DPRNT still ride it. It replaces *file delivery* only.

### The older path, for reference: network file drop

The right shape for "auto send" is **not DNC** — it is putting the file
where the control can already see it, so the operator's job collapses from
"set up a receive, then start" to "pick the file, then start."

Haas NGC controls have a Net Share feature (Settings 900–907 — CNC Network
Name, DHCP/static, IP, mask, gateway, workgroup — all already in
`cnc/haas_codes.go`). Two directions are possible in principle:

- **Machine mounts a share we serve.** zinc exports an SMB share; the Haas
  mounts it and the operator loads programs from the network device on the
  pendant. This is the well-trodden path in shops and requires no write
  access into the machine.
- **We write into the machine's own storage.** Depends entirely on whether
  this control exposes its User Data over SMB. Do not assume it does.

### VERIFIED 2026-08-12 — the control is on the network

The TM-2P is at **`192.168.20.248`** (static, set in UniFi), MAC
`00:1e:bf:01:5a:b3`. This is the *control itself*, not the Waveshare — the
bridge remains a separate device at `192.168.20.200:4196`.

Confirmed from zinc with the machine powered on:

| Probe | Result |
|---|---|
| ICMP | 0% loss, ~0.9 ms, TTL 128 |
| tcp/23 (telnet) | **open** |
| tcp/80 (http) | **open** — serves `<TITLE>HAAS Embedded Web/M-Net Server</TITLE>` |
| tcp/139, tcp/445 (SMB) | **closed** |
| tcp/21, 22, 443, 502, 4196 | closed |
| tcp/5051, 5000, 5025, 8080, 8082, 9000, 21328 | closed |

Three conclusions that settle the open questions above:

1. **This is the Classic Haas Control networking option ("M-Net"), not
   NGC.** The banner names it outright.
2. **We cannot write into the machine's storage.** It does not serve SMB —
   139 and 445 are closed. M-Net makes the control an SMB *client*, so the
   only workable direction is **zinc exports a share and the Haas mounts
   it**. The operator then loads programs from the network device at the
   pendant. That is the network-drop design; "write to its HDD" is off the
   table.
3. **There is no Q-code-over-Ethernet path on this control.** Every port
   associated with Haas MDC-over-network is closed, so the RS-232 bridge
   stays the only telemetry transport. The single-client constraint that
   `cnc/link.go` is built around is permanent, not incidental.

Still unverified, all requiring someone at the pendant:

- Settings 900–907 as the control actually has them (we know it is
  networked; we have not read back its own view of that config).
- Whether the M-Net settings for a remote share (server name / share path /
  user / password) exist on this firmware revision and what they are
  numbered.
- Setting 55 (Enable DNC from Device) and Setting 14 (handshake) — the DNC
  pre-conditions.

The embedded web server is worth a closer look on its own merits: it is a
second, independent read path that does **not** compete for the
single-client serial bridge. `GET /` returns an IFRAME shell pointing at
`view.html`; that page did not respond within 10 s on first probe, so what
it exposes is unknown. If it surfaces offsets or machine state, it is a far
better source for the periodic refresh below than Q600 macro reads.

---

## C. Nothing ever re-reads the tool table

Confirmed live 2026-08-12: with the machine on and the link healthy
(`connected: true` on `GET /api/displays/{id}`), the payload still carried
`last_updated: 2026-06-13T18:18:39Z`. The e-paper correctly showed
`[connected]` next to two-month-old geometry.

These are two independent facts and it is worth keeping them apart:

- **Connectedness** is live — a Q-code round-trip inside the staleness
  window (`cnc/link.go`, `Link.Alive`).
- **The tool table** is a persisted JSON dump on disk, written only when an
  operator clicks Read Tool Table (`POST /api/cnc/tool-table`). There is no
  background dumper. Nothing has ever refreshed it on its own.

So a display can be simultaneously connected and completely wrong, with no
indication of which numbers are live and which are archaeology.

### The fix

1. **Periodic background tool-table dump.** This was unaffordable under the
   old dial-per-query model — a full table read is a long burst of Q600s
   against a bridge already at ~75% saturation. With the persistent link
   owning the socket it is cheap and schedulable. Gate it on the link being
   alive, skip it entirely while a job is streaming (the socket is busy and
   the table cannot change mid-program anyway), and make the interval an
   operator setting alongside `BaselinePollSeconds`.
2. **Show the dump age wherever the numbers are shown.** The display already
   renders `updated <date>`; the dashboard and the pre-flight check should
   refuse to be quietly confident about a dump older than some threshold.
   Pre-flight in particular compares program T-references against this data
   — a stale table means a green pre-flight that proves nothing.
3. Only then is the three-way reconciliation in section A meaningful; it
   depends on "what the machine reports" being current.

---

## D. Presence detection — gate everything on "is the mill actually on?"

Now that the control has its own IP (`192.168.20.248`), there is a cheap
signal we never had: **ask the machine directly instead of inferring it from
the bridge.**

This matters because the Waveshare cannot tell us. It answers TCP on
`:4196` whether or not the mill behind it is powered — which is precisely
why `cnc/link.go` has to treat a silent controller as non-fatal. The bridge
being reachable says nothing about the machine.

### The mechanism

One **TCP connect to `192.168.20.248:80`**, short timeout, on an interval.
Nothing more. That is a single SYN against a port we have confirmed open.

Why that and not the alternatives:

- *ICMP ping* would be marginally lighter, but Go needs either raw sockets
  or unprivileged-ICMP to be enabled, and `filebrowser` runs as `admin`
  (non-root) on zinc. Not worth the deployment dependency.
- *Reading the ARP cache* is cheapest of all but unreliable — entries go
  STALE and linger after the device is gone, so it reports the past.
- A TCP connect needs no privileges and says something stronger than ICMP:
  the control's own stack is up and serving.

To be explicit, since it is the obvious worry: this is one connect to one
known-open port on a slow interval. It is not a scan, and the port sweep in
this document was a one-time identification step, not a design.

### This is a net *reduction* in traffic

Today, with the mill off, the baseline poller still fires a Q-code at the
bridge every `BaselinePollSeconds` and eats a full `queryTimeout` (3 s) of
silence for each one. Gating on presence replaces that with a single SYN per
interval and **no bridge traffic at all** while the machine is off. Fewer
packets, and no more three-second stalls in the link's service loop.

### Three states instead of one bool

`connected` currently collapses distinct failures into one flag. With a
presence check they separate, and each has a different fix:

| Presence (`:80`) | Q-code round-trip | Meaning |
|---|---|---|
| down | — | Mill is powered off. Don't touch the bridge. |
| up | failing | Control is on but not talking: Setting 143 off, serial cable, or bridge fault. |
| up | ok | Fully connected. |

The middle row is the one we cannot currently distinguish from the top row,
and it is the one that would have saved the original debugging session.

### The edge is the trigger

**down → up is the sync event.** The machine just came on; that is exactly
when its tool table is worth re-reading and exactly when the operator is
about to care. Firing a tool-table dump on that edge gets section C's
freshness without a polling timer doing speculative work all day:

- on presence edge up → wait for the link to report a live round-trip →
  dump the tool table once
- optionally a slow periodic re-dump on top, for tables edited at the
  pendant mid-shift

Interval and any periodic re-dump cadence are operator settings — pick them
from real behaviour on the shop floor, not from this document.

### Keep it small

The whole thing is a goroutine holding one bool with a mutex, an interval,
and a callback on the rising edge. It should not grow into a health
subsystem. If it needs more than roughly a hundred lines, the design drifted.

### Fallback if the control turns out to have no network

Not nothing — the operator step can still be shrunk:

- Keep the queue and pre-flight exactly as they are, but make the dashboard
  tell the operator, in order, the literal pendant keystrokes for the
  selected send method. Turns a remembered ritual into a checklist.
- Auto-arm: the moment the control reports it has entered a receive state
  (detectable via the Q104 mode poll the new baseline link already runs),
  start the queued stream without a second click.
