# CLAUDE.md — filebrowser-NC

Fork of `filebrowser/filebrowser`. **`gh pr create` defaults to the UPSTREAM parent**, because GitHub stores the fork relationship in repo metadata (the local `origin` doesn't override it). ALWAYS pass all three flags: `gh pr create --repo jasongainor/filebrowser-NC --base master --head jasongainor:<branch>`. (Once opened a CNC PR against public upstream by accident — embarrassing and unclosable.)

## Adding a Vue route → also add the titles map entry
When you add a route in `frontend/src/router/index.ts`, you MUST add the route name to the `titles` map in the same file. `router.beforeResolve` runs `t(titles[route.name])`; an unmapped name → vue-i18n `INVALID_ARGUMENT` (code 17) → blank page at navigation. The build AND tests pass — it's a runtime-only failure. Use an existing i18n key (add to `frontend/src/i18n/en.json` first if new). Incident: PR #119 route → hotfix #120.

## Unauthenticated handlers must hydrate `d.user`
A handler registered without `withUser` / `withAdmin` (firmware/kiosk endpoints) has `d.user == nil`. Anything touching per-user scope (`d.user.FullPath`, `toolTableDirAbs`, …) nil-derefs. Go's net/http recovers the panic by closing the connection with NO body — the symptom is `curl: (52) Empty reply from server` / browser `ERR_EMPTY_RESPONSE`, NOT a 500, and nothing in journalctl. Hydrate manually:

```go
if d.user == nil {
    u, err := firstAdminUser(d)
    if err != nil { return http.StatusInternalServerError, err }
    d.user = u
}
```

`firstAdminUser(d)` lives in `http/cnc_displays.go`. Incident: PR #119 → hotfix #122.

## Upstream sync
Squash-merging an upstream-sync PR breaks the merge-base (master stays "N behind" forever; the next merge re-applies the same commits with phantom conflicts). Repair with `git merge -s ours upstream/master` after proving content is applied (pure-upstream files byte-identical). Land it as a real merge commit, never squash. Prove content applied (pure-upstream files byte-identical) before the `-s ours` repair.

---
Host-local notes (if any) live in CLAUDE.local.md, gitignored; absence is normal.
@CLAUDE.local.md
