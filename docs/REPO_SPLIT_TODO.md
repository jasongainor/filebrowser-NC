# Should the CNC stack leave the filebrowser fork?

Open question, not a decision. Raised 2026-08-12 after the persistent-link
work, on the observation that this increasingly isn't a file browser with a
CNC page — it's a machine-monitoring product that happens to ship a file
browser.

## Where the weight actually sits

Rough line counts, backend only:

| | lines |
|---|---|
| `cnc/` (all ours) | ~8,600 |
| `http/cnc*.go` (all ours) | ~2,700 |
| `http/` total | ~7,000 |

So `http/` is about 39% ours by volume, and `cnc/` is a package upstream has
no concept of. Add the Vue side — machine dashboard, queue panel, toolpath
viewer, tool list, right rail, e-paper display config — plus the `pi-setup/`
provisioning and the `eterminal-display/` ESP32 firmware, neither of which
has anything to do with file browsing.

Upstream contributes: the file tree, auth/users, the share system, the
settings store, the HTTP scaffolding we hang the CNC router off.

## The case for splitting

- **Upstream sync is already a documented hazard.** `CLAUDE.md` carries a
  whole section on it: squash-merging a sync PR breaks the merge-base and
  the next merge re-applies everything with phantom conflicts, repaired only
  with `git merge -s ours`. That is a standing tax we pay for code that
  mostly doesn't touch upstream files.
- **`gh pr create` defaults to the upstream parent** because GitHub stores
  the fork relationship in repo metadata. This has already caused one CNC PR
  to be opened against public upstream by accident. A non-fork repo makes
  that class of mistake impossible rather than something we defend against
  with three mandatory flags on every PR.
- **The interesting surface is diverging fast** — persistent bridge link,
  presence detection, tool-library identity, network program drop. None of
  it has an upstream analogue or an upstream home.
- The e-paper firmware and the Pi provisioning are already effectively
  separate products living in the same tree.

## The case against

- **The file browser is load-bearing, not incidental.** The whole workflow is
  "drop an NC file in a folder, see it on the shop floor, send it." Auth,
  user scoping, shares, and the file tree are all genuinely used. Splitting
  means either vendoring filebrowser as a dependency or reimplementing a
  surprising amount of plumbing.
- **`http/` is genuinely interleaved.** The CNC handlers register on the
  same router and lean on upstream's user/auth middleware — including the
  `d.user` hydration gotcha documented in `CLAUDE.md`. That is not a clean
  seam today.
- Splitting costs real time and buys no new capability. Nothing on the
  roadmap is *blocked* by living in a fork.
- One deploy path currently covers everything (`cnc-autodeploy` on zinc,
  one binary, one service). Two repos means two of those, or a build that
  reassembles them.

## The shape a split would take, if we do it

Not "start a new repo and copy files." The realistic sequence:

1. **Make the seam real first, in place.** Get `cnc/` to depend on an
   interface for the handful of things it needs from filebrowser (user
   scope resolution, the settings store, file paths) rather than on
   concrete upstream types. That is worth doing even if we never split —
   it is the same work that would make `cnc/` testable without an HTTP
   stack.
2. **See what's left.** If after step 1 the CNC side touches upstream only
   through a small interface, the split is mechanical. If it doesn't, the
   split was never going to be clean and we have our answer.
3. Only then decide. Options at that point: a separate service that
   filebrowser proxies to, a Go module consumed by a thin fork, or a real
   fork-detach (`gh repo` fork relationship removed) keeping one tree.

Step 1 is the only part worth committing to now. It is useful unconditionally
and it converts an argument into a measurement.

## Decide it when

There's no urgency while upstream syncs stay rare. The trigger to revisit:
the next time an upstream merge costs more than an hour, or the next time
something in `cnc/` can't be built the right way because of where it lives.
