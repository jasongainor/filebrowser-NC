# Repo policy: this is OUR fork — never push to upstream

`jasongainor/filebrowser-NC` is a **permanently diverged downstream fork**.
All work merges **here**, never to the upstream `filebrowser/filebrowser`.

## Hard rules
- **Push only to `origin`** = `git@github.com:jasongainor/filebrowser-NC.git`.
- **Never** add an `upstream` push target or push to `filebrowser/filebrowser`.
- Open pull requests **into our own `master`** (fork → fork). On a GitHub
  fork, the web "Compare & pull request" page may default the base-repo
  dropdown to the upstream parent — **always confirm it reads
  `jasongainor/filebrowser-NC`**. The always-safe compare link is:

  `https://github.com/jasongainor/filebrowser-NC/compare/master...<branch>?expand=1`

- Fetching/pulling from upstream to **sync** is fine; only **pushing** is forbidden.

## Enforcement (wired up)
- **Git pre-push hook** — `.githooks/pre-push` rejects any push whose remote
  URL isn't our fork. Activate once per clone:
  ```sh
  git config core.hooksPath .githooks
  ```
- **`git config remote.pushDefault origin`** — a bare `git push` always
  targets our fork.
- **`.claude/settings.json`** — denies Claude Code from adding an upstream
  remote, pushing to upstream, or opening PRs via `gh` (we open PRs by hand).

If you clone fresh, run the `core.hooksPath` line above to re-arm the hook.
