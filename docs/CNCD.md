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
  }
}
```

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

Every path under `/api/files` and every path resolved for `/api/displays/{id}`
goes through `cncapi.RootResolver`, jailed to `--root`: a `..` (or any
combination that would land outside root) is rejected with an error rather
than silently clamped back to root.

## Not here yet, on purpose

This is the daemon's first cut — enough to prove the seam and unblock the
two documented external integrations, not full parity with filebrowser's
`/api/cnc/*` surface. Specifically missing, deferred to later work:

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
- **A UI.** `/api/files` exists to give a future UI something to build on,
  not because cncd renders one itself.

## Testing

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
