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

### The path worth pursuing instead: network file drop

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

### Fallback if the control turns out to have no network

Not nothing — the operator step can still be shrunk:

- Keep the queue and pre-flight exactly as they are, but make the dashboard
  tell the operator, in order, the literal pendant keystrokes for the
  selected send method. Turns a remembered ritual into a checklist.
- Auto-arm: the moment the control reports it has entered a receive state
  (detectable via the Q104 mode poll the new baseline link already runs),
  start the queued stream without a second click.
