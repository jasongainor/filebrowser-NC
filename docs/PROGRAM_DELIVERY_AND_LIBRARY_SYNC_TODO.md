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

### The unknown that gates all of it

**We have not verified that the TM-2P itself is on the network.** What is
confirmed on the shop LAN is the *Waveshare bridge* at `192.168.20.200:4196`
— that is a serial-to-TCP converter, not the control. The mill having an
Ethernet port, having the networking option enabled, and having a usable IP
are three separate facts, none of them checked.

The README mentions an operator workflow for programs "loaded from SD card /
Ethernet drop", which hints the control may have a network path already in
use — but that is a doc claim, not a measurement.

### Verification steps (needs the machine powered on)

1. At the pendant: Settings 900–907. Is there an IP? Is 901 (DHCP) on? Does
   the control have the networking option at all, or do those settings not
   exist on this firmware?
2. From zinc, with the mill on: `ping <machine-ip>`, then probe 139/445 to
   see whether it presents SMB.
3. Check Setting 55 and Setting 14 while standing there — the DNC
   pre-conditions above.
4. Record the control generation (Classic vs NGC) and firmware version.
   Everything above branches on it; a Classic control has no networking and
   the answer is "serial forever, operator action is mandatory."

Only after step 1–4 is it worth designing the drop. Until then this is
speculation, and the honest status of "auto-send to the machine" is:
**blocked on a five-minute check at the pendant.**

### Fallback if the control turns out to have no network

Not nothing — the operator step can still be shrunk:

- Keep the queue and pre-flight exactly as they are, but make the dashboard
  tell the operator, in order, the literal pendant keystrokes for the
  selected send method. Turns a remembered ritual into a checklist.
- Auto-arm: the moment the control reports it has entered a receive state
  (detectable via the Q104 mode poll the new baseline link already runs),
  start the queued stream without a second click.
