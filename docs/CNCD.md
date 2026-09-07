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
  "baselinePollSeconds": 15,
  "auth": {
    "smbAddress": "127.0.0.1:445",
    "domain": "",
    "sessionTTLHours": 168
  },
  "jobs": {
    "rootIsJobs": true,
    "autoBucket": true
  }
}
```

`jobs` is additive and optional, like `auth` — both knobs default to `true`
even when the key is absent entirely, so an existing install picks up the
bucketed-root behavior without touching this file. See `docs/JOB_FOLDERS.md`
for the folder-naming rule, the auto-bucket watcher, and how to turn either
knob off.

`auth` is additive and optional — an absent or zero-valued `auth` object
gets cncd's own defaults (`127.0.0.1:445`, empty domain, a one-week session
TTL). See "Sign-in" below.

Anything cncd mutates (currently nothing — see below) is written back to
this file, replacing it atomically (write to `.tmp`, rename over).

## Identity: machine-token bearer, or a Samba-backed session

cncd's `Authz` is decided one of two independent ways:

- Presenting the configured `machineToken` as `Authorization: Bearer <token>`
  — the m2m path (renishaw-builder, monitoring, etc).
- Carrying a valid session cookie from `POST /api/login` — the human path,
  described in "Sign-in" below.

Either grants `Authz.CanModify()` and `Authz.IsAdmin()` both true. Neither
present is read-only.

Routes that have a session fallback in filebrowser (`/api/cnc/state`,
`/api/cnc/qcode`, `/api/cnc/stream`) have no such fallback here — they
require the machine-token bearer outright, session or not. Missing or wrong
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

## Sign-in

There is no second user store for the web UI. Samba users are the users:
whatever password maps the share for the Haas control is the password that
signs into cncd's UI.

- On the Pi, `smbpasswd -a jason` creates (or resets) the Samba account
  `jason` authenticates with. That's the entire "user management" story —
  there is no cncd-side account table to keep in sync.
- The Haas control keeps mounting the share over guest SMB1, unaffected —
  sign-in only changes how the *web UI* authenticates, not the share mount
  the control uses.
- `POST /api/login` with `{"username", "password"}` validates the pair by
  performing an SMB2 (NTLM) session setup against the local Samba server —
  the same credential check `smbclient -U jason //127.0.0.1/share` would
  do — and immediately logs the SMB session back off. No share is mounted
  and no file is read; establishing the session is the authentication.
  Success sets an `HttpOnly`, `SameSite=Lax` session cookie holding an
  opaque random id (kept in memory only — a daemon restart invalidates
  every session) and returns `{"username", "admin": true, "modify": true}`.
  A single-operator box has no group/role model yet, so any authenticated
  Samba user gets full admin+modify.
- Failure (wrong password, unknown user, malformed body) is always `401`,
  after a constant ~300ms delay so timing can't distinguish "no such user"
  from "wrong password." Login is also rate-limited to 5 failures per
  minute per remote IP.
- `POST /api/logout` invalidates the session and clears the cookie.
  `GET /api/me` reports the current session's identity, or `401` if there
  is none or it's expired.
- The Samba address (default `127.0.0.1:445`, i.e. the Pi's own `smbd`),
  NTLM domain, and session TTL (default one week, so a shop-floor kiosk
  doesn't demand re-login every shift) are configured under `auth` in the
  config file — see below.
- No password is ever logged or kept past the SMB round-trip that checks
  it.

## Routes mounted today

| Method | Path | Notes |
|---|---|---|
| GET | `/healthz` | liveness probe |
| POST | `/api/login` | Samba-backed sign-in; sets the session cookie |
| POST | `/api/logout` | invalidates the session, clears the cookie |
| GET | `/api/me` | current session identity, or 401 |
| GET | `/api/cnc/state` | baseline metric snapshot; bearer required |
| POST | `/api/cnc/qcode` | one-shot macro-var query; bearer required — this is renishaw-builder's integration point |
| GET | `/api/cnc/stream` | WS status/event feed; bearer required |
| GET | `/api/displays/{id}` | firmware endpoint — the ESP32 e-paper display's integration point; per-display token gate |
| GET | `/api/files?path=` | list a directory (or stat a file) under `--root`; open read |
| PUT | `/api/files?path=` | upload bytes to a path under `--root`; bearer or session required |
| DELETE | `/api/files?path=` | remove a single file under `--root`; bearer or session required, refuses directories |
| POST/GET/DELETE | `/mcp` | MCP server (streamable HTTP) — machine state, tool table, tool reconciliation, preflight, program listing; bearer required. See `docs/MCP.md`; also reachable over stdio via `cncd mcp-stdio`. |

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
| GET | `/api/jobs` | open | job-folder summary + unfiled root files — see `docs/JOB_FOLDERS.md` (not to be confused with `/api/cnc/jobs` above, which is run *history*, not the folder layout) |
| POST | `/api/jobs` | modify | create a job folder ahead of time; idempotent |
| POST | `/api/jobs/file` | modify | file one loose root file into its job folder by hand |
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

- **Group/role mapping for sessions.** Every signed-in Samba user is
  currently full admin+modify (see "Sign-in" above) — there's a `TODO` in
  `cncd/auth.go`'s `sessionAuthz` for narrowing this once more than one
  operator matters.
- **Admin CRUD over HTTP**: machine settings, display management, tool-table
  edit/history/diff, the send queue, auto-send, notifications, the tool
  library. These all exist in `cnc/` and are reachable from Go — they're not
  wired to cncd's router yet because the right authz story (who gets to
  reconfigure a headless daemon with no login) needs a decision, not just a
  bearer check.
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

`cncd`'s own tests (`cncd/*_test.go`) build a real `*cnc.Registry` against
an in-memory config `Store` and a temp-dir root, and drive the router with
`httptest` — no real machine, network, or filebrowser dependency involved.
Machines in tests are configured with an empty `Host`, which makes
`cnc.Streamer.resolveMachine` return `ErrConfigMissing` before any dial is
attempted, so registering a machine in a test never reaches the network.

`cncd/login_test.go` covers sign-in the same way: login/logout/session-me,
the wrong-password/rate-limit/expiry paths, and both the session and bearer
gates on `/api/files`. None of it talks to a real Samba server — `login.go`
defines an `Authenticator` interface the SMB implementation satisfies, and
tests inject a fake (`fakeAuthenticator`) plus an injectable clock/sleep, so
the constant-delay and TTL-expiry behavior is exercised without a real
network round-trip or an actual 300ms sleep per case. The real SMB2
round-trip against an actual Samba server is **not** exercised by this test
suite — see the PR description for what remains to verify on real Pi
hardware.
