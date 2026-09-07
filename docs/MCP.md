# cncd as an MCP server

cncd runs an [MCP](https://modelcontextprotocol.io) server in the same
process as the HTTP daemon (`cncd/mcp`, mounted by `cncd/router_mcp.go`).
The box itself is the MCP server: a Fusion-side CAM session (or any MCP
client) can ask it directly "what tools are in the machine right now, at
what offsets, and does my program's tool list match them" before a program
is ever sent — a swap or a too-short tool caught here costs nothing; caught
at the machine it costs a crash.

Every tool is read-only. Nothing here starts a job, edits the tool table,
or changes settings.

## Transports

### Streamable HTTP (the daemon's own port)

Mounted at `/mcp` on the same address `--listen` binds cncd's HTTP API to,
guarded by the identical `Authorization: Bearer <machineToken>` check every
other bearer-only cncd route uses (see `docs/CNCD.md`). Missing or wrong
bearer is `401`; there is no read-only fallback for `/mcp` the way there is
for `GET /api/files` — the same "no session concept" reasoning `/api/cnc/state`
already documents applies here too.

Point an MCP client that speaks streamable HTTP at:

```
http://<pi-host>:8080/mcp
Authorization: Bearer <machineToken>
```

Example client config (the shape most MCP-aware tools expect):

```json
{
  "mcpServers": {
    "cncd": {
      "url": "http://10.0.0.50:8080/mcp",
      "headers": { "Authorization": "Bearer <machineToken>" }
    }
  }
}
```

### stdio (`cncd mcp-stdio`)

For clients that spawn a local subprocess instead of dialing HTTP (Claude
Desktop's `mcpServers` config, Fusion's own MCP client if it works this way):

```
cncd mcp-stdio --root /srv/cnc-share --config /etc/cncd/config.json
```

Same `--root` / `--config` flags as the normal daemon invocation, same tool
set, same `cncd/mcp` wiring underneath (`cncd.NewMCPServer`) — just no HTTP
server started at all. There is no bearer check on this path: a subprocess
this process's own parent spawned directly is its own trust boundary, the
same reasoning `docs/CNCD.md` gives for `GET /api/files` being open on the
LAN-only HTTP surface.

Example client config:

```json
{
  "mcpServers": {
    "cncd": {
      "command": "cncd",
      "args": ["mcp-stdio", "--root", "/srv/cnc-share", "--config", "/etc/cncd/config.json"]
    }
  }
}
```

## Tools

All five tools take an optional `machine_id`; omit it to use the daemon's
default (first configured) machine. Every tool result carries both a
structured JSON payload and a short plain-text summary, so clients that
only render text still get something useful.

### `machine_state`

Current machine state: mode, current tool, running program + parts count
(via the `status_combined` metric), spindle RPM, machine/work positions, G54
offsets, and whether the bridge link is connected. This is the same data
`GET /api/cnc/state` serves, wrapped with machine identity and two
convenience flags (`connected`, `job_running`) so a client doesn't have to
parse raw Q-code strings itself.

```json
{ "name": "machine_state", "arguments": {} }
```

```json
{ "name": "machine_state", "arguments": { "machine_id": "m1" } }
```

### `tool_table`

Live tool-table pockets: T number, geometry length/diameter, wear, the
effective (geometry + wear) values, the tool-life/probe counter where a
machine's macro range for it has been confirmed (see
`docs/TOOL_LIFE_RESEARCH.md`), and when the table was last read. Returns the
last persisted dump by default.

```json
{ "name": "tool_table", "arguments": {} }
```

Set `refresh: true` to force a live read off the control instead. This is
only attempted when the machine is connected and not mid-job — otherwise the
persisted dump comes back unchanged with `refresh_note` explaining why, so a
refresh request never loses data. A 30-slot live read takes roughly 18
seconds; the tool call blocks until it finishes or the per-slot budget runs
out.

```json
{ "name": "tool_table", "arguments": { "refresh": true } }
```

### `check_tools`

The tool Jason asked for by name: reconcile a program's tool list against
the machine's tool table *before* programming continues. Takes either a
full `.gmw.json` sidecar document or a simplified list — use whichever you
have on hand.

Simplified list:

```json
{
  "name": "check_tools",
  "arguments": {
    "tools": [
      { "t": 12, "diameter": 0.375, "flute_length": 1.0, "max_depth": 0.6 },
      { "t": 1, "diameter": 0.5, "flute_length": 1.04, "stickout_length": 2.0, "max_depth": 1.0 }
    ],
    "clearance": 0.1
  }
}
```

Full sidecar (see the `program_identity` resource for the schema):

```json
{
  "name": "check_tools",
  "arguments": {
    "sidecar": {
      "schema_version": 1,
      "job": "J000020",
      "tools": [
        { "t_number": 5, "diameter": 0.25, "flute_length": 0.75, "max_depth": 1.0 }
      ]
    }
  }
}
```

Response includes the full per-tool reconciliation (`match` / `moved` /
`missing`, diameter warnings, length-insufficient flags with the actual
numbers behind each) plus a one-paragraph `summary` a CAM session can act on
directly, e.g.:

> T12 not loaded: load into pocket 12 and touch off. T1 needs 1.1 reach,
> flute is 1.04: choose a longer tool or reduce depth.

### `preflight_program`

Parses an NC file's tool references and compares them against the latest
tool-table read: per-tool `ok` / `warn` / `empty` / `offline` / `missing`
status, the starting tool, a swap warning when the controller's current
spindle tool differs from the program's first tool, and — when the file
carries a `(GMW-ID V1)` header — the sidecar-based reconciliation as an
additional `identity` section (see `docs/PROGRAM_IDENTITY.md`).

```json
{
  "name": "preflight_program",
  "arguments": { "file_path": "jobs/J000020/OP10.nc" }
}
```

`file_path` is relative to the daemon's served root (`--root`); it's
resolved through the same root-jailed `cncapi.PathResolver` `/api/files`
uses, so a `..` component can't escape the share.

### `list_programs`

Lists files on the share with an identity badge (`job` / `operation` /
`part` / `sha_status`) wherever a file carries a GMW header. Un-annotated
files are listed too, with `has_identity: false` — normal for anything not
run through the GMW post yet, not an error. Tool-table dump directories and
`.gmw.json` sidecars are excluded (dumps aren't programs; a sidecar's
identity is already reported through the NC file it sits next to).

```json
{ "name": "list_programs", "arguments": {} }
```

```json
{ "name": "list_programs", "arguments": { "path": "jobs/J000020" } }
```

## Resource: `program_identity`

`docs://cncd/program-identity` exposes the full text of
`docs/PROGRAM_IDENTITY.md` — the `GMW-*` header format, the `.gmw.json`
sidecar JSON Schema, and the Fusion post-processor snippet that writes
both — embedded into the `cncd` binary at compile time (`docs/embed.go`), so
it's available even on a headless Pi with no checked-out repo next to it.

A CAM session should read this resource once, up front, before constructing
a `check_tools` `sidecar` argument or relying on `preflight_program`'s
`identity` section.

## What a Fusion session should do before posting

1. Call `machine_state` to confirm the box is reachable and idle.
2. Call `tool_table` (optionally `refresh: true`) to see what's actually
   loaded right now.
3. Read the `program_identity` resource once per session, if you haven't
   already, so the sidecar you build matches the schema `ReconcileTools`
   expects.
4. Call `check_tools` with the operation's planned tool list (simplified or
   full sidecar) — fix anything reported as `missing`, `moved`, or
   length-insufficient *before* generating G-code, not after.
5. Once the post has written the `.nc` file (and its `.gmw.json` sidecar) to
   the share, call `preflight_program` against it as a final check — this
   catches drift between what `check_tools` saw during planning and what's
   actually in the machine by the time the file lands.
6. Use `list_programs` to confirm the file landed where expected and carries
   the identity badge you expect (right job/operation/part, `sha_status` not
   `mismatch`).

None of these tools send anything to the machine. Sending the program is
still an operator action on the daemon's existing HTTP/UI surface — this
MCP server is a pre-flight advisor, not a remote control.
