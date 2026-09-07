# cncd — the standalone CNC daemon

`cmd/cncd` is a headless binary that serves the CNC API from one directory,
with no filebrowser users, bolt DB, or Vue frontend behind it. It's step one
of the split proposed in `docs/REPO_SPLIT_TODO.md`: "make the seam real in
place first." The seam is `cncapi.PathResolver` / `cncapi.Authz`
(`cncapi/seam.go`) — the same two interfaces `http/cnc*.go` now goes through
instead of reaching into a filebrowser `*users.User` directly
(`http/cnc_seam.go`). cncd is the second implementation of that seam: a
root-jailed directory instead of a user's scoped share, a bearer-token check
instead of a session.

## Running it

```
cncd --root /srv/cnc-share --config /etc/cncd/config.json --listen :8080
```

- `--root` — the one directory cncd serves. This is meant to be the same
  Samba share filebrowser used to browse; cncd doesn't touch Samba itself
  (see "Not here yet" below).
- `--config` — path to a JSON file. Created automatically (zero-valued) on
  first run if it doesn't exist, so a fresh Pi doesn't need a hand-authored
  file to boot.
- `--listen` — address to bind, default `:8080`.

## Config file shape

The config file is exactly the `settings.Cnc` document filebrowser used to
keep in its bolt DB — machines, displays, discord, machine token, baseline
poll interval:

```json
{
  "machines": [
    {"id": "m1", "name": "Haas VF-2", "host": "10.0.0.50", "port": 4196}
  ],
  "machineToken": "…",
  "discord": {},
  "displays": [
    {"id": "d1", "machineId": "m1", "name": "Shop floor e-paper"}
  ],
  "baselinePollSeconds": 15
}
```

Anything cncd mutates (currently nothing — see below) is written back to
this file, replacing it atomically (write to `.tmp`, rename over).

## Identity: bearer token only, for now

cncd has no login yet. The one thing it can check is whether a request
presents the configured `machineToken` as `Authorization: Bearer <token>`:

- Matching bearer → `Authz.CanModify()` and `Authz.IsAdmin()` both true.
- No bearer, wrong bearer, or no token configured at all → read-only.

Routes that have a session fallback in filebrowser (`/api/cnc/state`,
`/api/cnc/qcode`, `/api/cnc/stream`) have no such fallback here, since there
is no session concept — they require the bearer outright. Missing or wrong
bearer on those routes is `401`, the same status fbhttp already returns for
a bad bearer there. `GET /api/displays/{id}` keeps its own, independent gate:
each Display carries an optional `token` field, checked the same way
filebrowser checks it (empty = LAN-permissive, matching = required).

## Shared handler bodies (`cncapi`)

Everything below the state/qcode/stream/files/displays trio (which predates
this section) is implemented once, in `cncapi/handlers_*.go`, and called from
both hosts:

- `http/cnc*.go` (fbhttp) wraps a shared function with `withUser`/`withAdmin`,
  `renderJSON`, and its existing mux routes — behavior for existing
  (authenticated) filebrowser clients is unchanged.
- `cncd/router_cnc.go` wraps the *same* function with a bearer-derived
  `cncapi.Authz` and cncd's own JSON helpers.

`cncapi.Deps` (`cncapi/deps.go`) bundles what a shared handler needs: the live
`*cnc.Registry`, a `PathResolver`, an `Authz`, and a `SettingsStore` (read/
mutate `settings.Cnc`, persisted however the host does that — the whole bolt
`settings.Settings` document on filebrowser, a single JSON file on cncd).
Route table and gating below reflect this; see each `cncapi/handlers_*.go`
file's doc comment for which `http/cnc*.go` handler it mirrors.

## Routes mounted today

Gating collapses filebrowser's three-tier model (admin / modify / any logged-in
user) onto cncd's two-tier bearer: **admin** and **modify** map to
`Authz.IsAdmin()` / `Authz.CanModify()` (today, both are "bearer matches the
configured machine token"); filebrowser's plain "any logged-in user" tier maps
to **open** (no auth token needed at all — the same LAN-permissive posture as
`GET /api/files` and `GET /api/displays/{id}` with no per-display token set).

| Method | Path | Gate | Notes |
|---|---|---|---|
| GET | `/healthz` | open | liveness probe |
| GET | `/api/cnc/state` | bearer (no session fallback) | baseline metric snapshot |
| POST | `/api/cnc/qcode` | bearer (no session fallback) | one-shot macro-var query — renishaw-builder's integration point |
| GET | `/api/cnc/stream` | bearer (no session fallback) | WS status/event feed |
| GET | `/api/displays/{id}` | per-display token | firmware endpoint — the ESP32 e-paper display's integration point |
| GET/PUT/DELETE | `/api/files?path=` | open / modify / modify | root-jailed file API |
| GET | `/api/cnc/queue` | open | list the per-machine send queue |
| POST | `/api/cnc/queue` | modify | add a file to the queue |
| PATCH | `/api/cnc/queue` | modify | reorder the queue |
| DELETE | `/api/cnc/queue/{id}` | modify | remove a queued item |
| POST | `/api/cnc/queue/{id}/promote` | modify | mark an item "running" (manual counterpart to the streamer's O-number auto-match; no filebrowser precedent — new) |
| GET | `/api/cnc/status` | open | streamer status + attachment/recovery flags |
| POST | `/api/cnc/start` | modify | start a send (honors `RequirePreflight`) |
| POST | `/api/cnc/stop` | modify | stop the running job |
| POST/DELETE | `/api/cnc/attach` | modify | mark/clear "this file is what's running" |
| POST | `/api/cnc/preflight` | modify | tool-table + Identity check before a send — **POST with a JSON body** on cncd, unlike fbhttp's `GET` + `?file_path=` (no legacy query-string client to stay compatible with here) |
| POST | `/api/cnc/auto-send` | modify | preflight + start in one round trip |
| POST | `/api/cnc/tool-table` | admin | live controller read, persists a dump |
| GET | `/api/cnc/tool-table` | open | latest persisted dump, or `204` |
| GET | `/api/cnc/tool-table/history` | open | list persisted dumps |
| POST | `/api/cnc/tool-table/edit` | modify | local-only offset override |
| GET | `/api/machines/{id}/toollist` | open | reconciled tool-list view (also backs the firmware endpoint) |
| GET | `/api/cnc/tool-library` | open | Fusion 360 tool-library export |
| PUT | `/api/cnc/tool-library` | admin | replace the tool library |
| GET | `/api/cnc/machines` | open | configured machines + default id |
| GET | `/api/cnc/settings` | admin | the full `settings.Cnc` document, verbatim |
| PUT | `/api/cnc/settings` | admin | replace it — treated opaquely (machines get `cncapi.NormalizeMachines` validation; every other field, including ones added later, round-trips unvalidated) |
| POST | `/api/cnc/settings/token` | admin | mint a new machine token |
| GET | `/api/cnc/jobs` | open | job history |
| GET | `/api/cnc/jobs/stats` | open | windowed job aggregates |
| GET | `/api/cnc/codes/lookup` | open | Haas alarm/setting/parameter lookup |
| GET | `/api/cnc/codes/search` | open | free-text code search |
| GET | `/api/cnc/host-stats` | open | Pi health snapshot |
| POST | `/api/cnc/recovery/ack` | modify | acknowledge a dirty-stop recovery |
| GET | `/api/cnc/displays` | admin | list displays (includes tokens — admin-only, unlike the other `open` list routes) |
| POST | `/api/cnc/displays` | admin | create a display |
| PUT/DELETE | `/api/cnc/displays/{id}` | admin | update/remove a display |

Every path resolved against `--root` (`/api/files`, tool-table dump
directories, `/api/displays/{id}`'s tool-list build) goes through
`cncapi.RootResolver`: a `..` (or any combination that would land outside
root) is rejected with an error rather than silently clamped back to root.

### Settings shape on cncd vs. filebrowser

filebrowser's `/api/cnc/settings` keeps a narrower wire shape
(`cncSettingsBody`, `http/cnc.go`) with legacy `haasHost`/`haasPort`/`cameraUrl`
mirror fields a pre-multi-machine UI still POSTs, and Discord lives on its own
`/api/cnc/notifications` endpoint. cncd has no legacy UI to stay compatible
with, so its `/api/cnc/settings` just round-trips `settings.Cnc` whole —
machines (including the `serial` block), displays, discord, machine token,
and anything added to the struct later — with no separate notifications
route.

## Not here yet, on purpose

Specifically missing, deferred to later work:

- **A login screen.** A later step validates web logins against Samba
  credentials on the Pi. Until then, the only identity beyond the
  machine-token bearer is per-display tokens.
- **Diagnostics / one-off admin actions** that exist in `http/cnc.go` but
  weren't part of this pass: `/api/cnc/check` (bridge/controller connectivity
  probe), `/api/cnc/siblings` (3D-model/drawing sibling lookup), `/api/cnc/probe-tools`
  and `/api/cnc/probe-tool-life` (macro-range probing), `/api/cnc/chapters`
  (NC operation-header TOC), `/api/cnc/tool-table/diff`, `/api/cnc/notifications`
  (Discord config + test-send), and the tool-library per-slot lookup /
  delete (`/api/cnc/tool-library/slot/{n}`, `DELETE /api/cnc/tool-library`).
  These all exist in `cnc/` and are reachable from Go — wiring them up is
  the same shape of work as this pass, just not part of the UI prototype's
  immediate needs.
- **Serial passthrough** beyond the existing Haas RS-232↔TCP bridge protocol
  that `cnc/link.go` already speaks.
- **A UI.** `/api/files` and the routes above exist to give a future UI
  something to build on, not because cncd renders one itself.

## Testing

`cncd`'s own tests (`cncd/*_test.go`, including `router_cnc_test.go` for the
routes in this pass) build a real `*cnc.Registry` against an in-memory config
`Store` and a temp-dir root, and drive the router with `httptest` — no real
machine, network, or filebrowser dependency involved. Machines in tests are
configured with an empty `Host`, which makes `cnc.Streamer.resolveMachine`
return `ErrConfigMissing` before any dial is attempted, so registering a
machine in a test never reaches the network. Because of that, a mutating
route's "happy path" test typically asserts the request cleared the authz
gate and reached real business logic (a 400/409 from the offline stub, never
a 403) rather than a full success — there's no real controller in the loop to
succeed against.

One gotcha specific to the queue routes: `cnc.NewRegistry` always backs its
`QueueStore` with the process-wide default directory
(`resolveQueueDir()`, `cnc/queue.go`) — there's no way to inject a temp dir
per test — so every queue test shares one on-disk `m1.json` across runs.
`router_cnc_test.go`'s `resetQueue` helper clears it before (and after) each
queue-touching test.
