# Run reporting — the daemon's side of gmw-mes machine time

`cnc/reporter.go` posts program-run lifecycle data to gmw-mes's
`machine.run` API (gmw-mes repo: `docs/machine-runs.md`,
`gmw_mes/api/machine_runs.py`) so setup/machine time reaches Carbon
instead of staying pinned at the placeholder zero. This is the Go
side; see that doc for what gmw-mes does with what it receives.

## What is sent, and when

`cnc.Reporter` (a `Registry`-owned singleton, alongside the Discord
`Notifier` — see `cnc/registry.go`) subscribes to each machine's event
feed and translates it into four calls:

| Trigger | Call | Notes |
|---|---|---|
| A streaming job starts, or the daemon attaches to a program the controller is already running (SD card / USB / Ethernet drop) | `POST /api/machine/runs` | Identity (`job_readable_id`, `operation_ref`, `part_id`, `program_sha256`) comes from `ParseIdentity` of the file, when one is available — see below. `o_number` comes from the aggregator's `status_combined` (Q500) metric, independent of whether identity resolved. |
| The `status_combined` metric's `PARTS` field changes | `POST /api/machine/runs/{run_id}/events` `{type: "parts"}` | Only fires when the run actually has a `run_id` (i.e. the create call already succeeded). |
| `HaasLastError` newly appears on the streamer's status | `POST .../events` `{type: "alarm"}` | |
| A `"log"` event at level `error` | `POST .../events` `{type: "error"}` | Also flags the run so an attach-only close (no `job_history` row to consult) reports outcome `error` instead of `unknown`. |
| The job/attach ends | `PATCH /api/machine/runs/{run_id}` | `outcome` for a real job comes from `cnc/job_history.go`'s own `completed`/`stopped`/`error` classification (the streamer's `run()` defer appends that row before the "running false" status event ever fires); an attach-only close falls back to `error` (last event was an error) or `unknown`. |

### Identity: only when a file is actually available

`job_readable_id`/`operation_ref`/`part_id`/`program_sha256` are only
populated when the Reporter can get its hands on the file's bytes:

- **Real streaming job** — `Start()` writes the active-job marker
  (`cnc/recovery.go`, `active_job_<machine>.json`) *before* the
  "running" status event fires, and that marker carries `AbsPath`. The
  Reporter reads it back, opens the file, and runs `ParseIdentity`
  (`cnc/program_identity.go`) on it. `program_sha256` is lower-cased
  before sending — gmw-mes's schema requires lowercase hex,
  `ParseIdentity.ComputedSHA256` is upper.
- **Attach** — `AttachedFile` (`cnc/streamer.go`) is share-relative,
  not an absolute filesystem path, and there is no per-user scope
  resolver inside `cnc` (that lives in `http/`, request-scoped). An
  attach-opened run is therefore reported with only `o_number` and
  `file_name` — no GMW-* identity. This matches the gmw-mes side's own
  rule: a missing/unresolved identity is not an error, the run is
  still stored and reported unlinked.

A program with a GMW header that fails to parse, or has no header at
all, degrades the same way — the run still opens, just without those
fields.

## Config

```jsonc
// settings.Cnc.Reporting
{
  "reporting": {
    "gmwMesUrl": "http://127.0.0.1:5401",
    "tokenEnv": "GMW_MES_BOT_TOKEN",
    "machineIds": { "haas-vf2": "mill-1" }
  }
}
```

- `gmwMesUrl` — base URL of the gmw-mes instance. **Empty disables
  reporting entirely** — every `Reporter` hook checks this first and
  returns immediately, so an existing install with no `reporting`
  block behaves exactly as it did before this feature landed.
- `tokenEnv` — the *name* of an environment variable holding the
  `X-Bot-Token` bearer, not the token itself. Defaults to
  `GMW_MES_BOT_TOKEN` (`settings.DefaultReportingTokenEnv`). The token
  value is read via `os.Getenv` at send time, on every request — it is
  never written to `settings.json`, never round-tripped through the
  admin UI, and never appears in a log line or an error string. Set it
  in the daemon's own environment (systemd `EnvironmentFile=`, or
  equivalent), the same way any other bot credential on this box is
  handled.
- `machineIds` — optional map from this install's own `Machine.ID` to
  the machine id gmw-mes should see. A machine absent from the map
  reports its own registry id verbatim.

## Resilience

- Every request has a 5 s timeout and one retry (short backoff) on a
  network error or `5xx`; a `4xx` is not retried (the request itself
  is wrong, not a bad moment for gmw-mes). After the retry, the
  request is logged and dropped — a report that never reaches gmw-mes
  is gone, the same trade `cnc/notify.go` makes for a missed Discord
  post.
- Each machine gets its own bounded queue (100 pending reports) drained
  by a single worker goroutine, so a run's create always lands before
  its events and its close, and a slow or unreachable gmw-mes can never
  block the streamer's own event loop. A full queue drops the newest
  report rather than blocking.
- **Restart recovery.** The in-flight run (if any) is mirrored to
  `<state>/active_run_<machine>.json` next to the existing
  `active_job_<machine>.json` marker (`cnc/recovery.go`). If the daemon
  restarts mid-run, the next `Watch` call reads that marker and closes
  the run immediately with outcome `unknown` (gmw-mes accepts
  `unknown` alongside its own `completed`/`stopped`/`error`
  vocabulary) rather than leaving it open at gmw-mes forever.

## What this does not do

No Carbon call happens here at all — this is strictly the daemon
posting floor data to gmw-mes. Turning a closed, linked run into a
Carbon `productionEvent` is entirely gmw-mes's job
(`gmw_mes/carbon/production_events.py`), gated by its own
`CARBON_PRODUCTION_EVENTS` flag, off by default. See the gmw-mes repo's
`docs/machine-runs.md` for that side.

The Reporter also never sends the program's own bytes
(`program_base64` in the gmw-mes API is optional) — only the identity
fields derived from it. Nothing here changes if that's added later;
it's just not needed for machine time to reach Carbon.
