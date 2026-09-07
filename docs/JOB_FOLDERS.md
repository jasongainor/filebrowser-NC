# Job folders — bucketing the served root by job

Written 2026-09-07. Answers Jason's ask directly: "I want files bucketed by
job. I want the machine to mount and just see each job in a folder; right
now there's the cnc top folder and it is an extra click to get in it."

## The shape

The served root — `--root` for cncd, `$SHARE_PATH` for the pi-setup
installer, `[cnc]` on the Samba side — **is** the jobs root. There is no
extra top-level folder to click through first:

```
<share root>/
  J000020 - F-BRACKET-REV-C/
    OP10.nc
    OP10.nc.gmw.json
    OP20.nc
    OP20.nc.gmw.json
  J000021 - HOUSING-REV-A/
    OP10.nc
  loose-file-with-no-header.nc      ← stays exactly here, forever
  cnc-tool-tables/                  ← unrelated: tool-table dumps, never a job
```

There is deliberately **no `_INBOX` folder**. The root itself is the inbox —
a file with no identity header just stays where it landed, which is also
zero extra folders for the common case of a program someone edited by hand
outside the GMW post pipeline.

## Naming rule

One folder per job, named from the program identity header
(`docs/PROGRAM_IDENTITY.md`'s `(GMW-JOB J000020 OP10)` / `(GMW-PART
F-BRACKET-REV-C)`):

```
<JOB> - <PART>
```

space-dash-space, e.g. `J000020 - F-BRACKET-REV-C`. The operation id
(`OP10`) is not part of the folder name — every operation of a job shares
one folder, since that's the unit an operator thinks in ("today I'm running
job 20") even though the Fusion post treats each operation as its own file.

Both tokens are sanitised the same way, independently, before being joined:

- Upper-cased.
- Every character outside `A-Z 0-9 SPACE . - _` is replaced with `_` (runs
  of replaced characters collapse to a single `_`, and stray separators are
  trimmed off both ends) — the same restricted set
  `docs/PROGRAM_IDENTITY.md` already requires of header content, chosen
  because it's simultaneously legal on FAT32 (the long-filename charset,
  more or less — this is the conservative subset that's also safe on the
  *8.3* fallback name Windows/DOS tooling still generates alongside an LFN),
  SMB1 (what the Haas control actually speaks — see
  `docs/PROGRAM_DELIVERY_AND_LIBRARY_SYNC_TODO.md`), and the Haas control's
  own file-list rendering, which is pickier about punctuation than a modern
  OS file picker.
- Truncated to 32 characters — short enough to read on the control's
  single-line file list without wrapping or eliding.
- No part (missing, or nothing left after sanitising): the folder is named
  from the job id alone, e.g. `J000020`.
- No usable job id at all (a malformed or absent `GMW-JOB`): there is
  nothing to bucket by, so the file is left at the root exactly like a file
  with no header at all.

Implementation: `cncd.JobFolderName(job, part string) string` in
`cncd/jobs.go`, with a sanitising-table test in `cncd/jobs_test.go`.

## How a file gets into its folder

Three ways, in order of how automatic they are:

1. **The auto-bucket watcher** (`cncd.RunAutoBucketWatcher`) polls the root
   every 5 seconds. Any loose file directly at the root that carries a GMW
   header and has been stable (unchanged mtime) for at least 2 seconds gets
   moved into its job folder, creating the folder if needed. The 2-second
   stability window exists because a post may still be writing the file —
   moving it out from under an in-progress write would be worse than
   leaving it a few seconds longer.
2. **`POST /api/jobs/file {"path": "/whatever.nc"}`** files one specific
   loose root file by hand, immediately, without waiting for the next poll
   — useful right after posting if you don't want to wait out the interval.
   Returns `400` if the file carries no header, or if `path` isn't a loose
   file sitting directly at the root (already-organized files are never
   touched — see below).
3. **Nothing else.** A file already inside a job folder, or inside any
   other folder a human made by hand, is never moved, and nothing is ever
   deleted — every move is a filesystem rename, never a copy-then-delete.

A file's `.gmw.json` sidecar (`docs/PROGRAM_IDENTITY.md`) always moves
alongside its NC file, in both paths above.

`POST /api/jobs {"job": "J000020", "part": "F-BRACKET-REV-C"}` creates a
job folder ahead of time (idempotent — calling it again just confirms the
resolved name) for a case where you want the folder to exist before the
first post lands.

`GET /api/jobs` lists every top-level job folder with a summary (program
count, sidecar count, newest file, a rolled-up `sha_status`, and the most
recent run recorded against it in `cnc/job_history.go`, matched by file-path
prefix) plus every loose root file under `"unfiled"`.

## Turning it off

`settings.Cnc.Jobs` (config key `jobs`) has two knobs, both defaulting to
**on** — an install with no `jobs` key in its `config.json` at all still
gets the bucketed-root behavior, which is the whole point of the feature
(nobody should have to hand-edit JSON to get the one-click file list Jason
asked for):

```json
{
  "jobs": {
    "rootIsJobs": true,
    "autoBucket": true
  }
}
```

- `"rootIsJobs": false` restores pre-feature behavior entirely: the
  auto-bucket watcher never runs. (`GET/POST /api/jobs` and
  `POST /api/jobs/file` stay reachable either way — they're read-only or
  operator-invoked, so leaving them mounted doesn't change what "today"
  looks like on its own; it's the *unattended* moving of files that this
  flag exists to gate.)
- `"autoBucket": false` (with `rootIsJobs` left on) disables just the
  watcher — job folders, `GET /api/jobs`, and manual filing via
  `POST /api/jobs/file` all still work; nothing moves a file without being
  asked.

Both restarts required to take effect — cncd reads config once at startup,
same as every other `settings.Cnc` knob today.

## What the Haas control sees

**Everything in this section should be verified at the pendant** — it's
written from the Haas Next Generation Control's documented `LIST PROGRAM`
behavior and the existing Net Share investigation in
`docs/PROGRAM_DELIVERY_AND_LIBRARY_SYNC_TODO.md`, not from having watched
this exact layout live on a machine yet.

- `LIST PROGRAM` shows tabs for `MEMORY | USB DEVICE | HARD DRIVE`, plus
  `NET SHARE` once Settings 900–915 are configured and F1 has been pressed
  (see `docs/PROGRAM_DELIVERY_AND_LIBRARY_SYNC_TODO.md`'s Net Share section
  for the full settings table and the "press F1 or it does nothing" gotcha).
  Whichever tab points at the share (Net Share for the FNC path, USB DEVICE
  for the mass-storage-gadget path pi-setup provisions) now shows job
  folders as entries in that tab's file list, directly — **not** behind an
  extra "cnc" or similar folder, because the served root itself is the
  jobs root.
- **Verify at the pendant:** highlighting a folder entry and pressing
  `ENTER` (or the right-arrow key, depending on firmware) descends into it;
  some means of going back up (an `UP DIRECTORY` line, or `F4`) should be
  present. This is standard classic-control file-list behavior but hasn't
  been confirmed against this specific job-folder layout yet.
- **Verify at the pendant:** how the control alphabetizes folders vs. files
  in a mixed listing (folders first? interleaved?) — matters for how
  quickly an operator finds today's job among older ones sitting at the
  root as loose files.
- **Verify at the pendant:** the 32-character folder name limit chosen here
  is conservative relative to FAT32 long filenames (255 chars) — confirm
  the control's own file-list column doesn't truncate or wrap even a
  full-length `J0000NN - PART-NAME-HERE` name before assuming a longer
  limit could safely be used later.
- Selecting a program (`FNC` mode, when running from Net Share or USB
  rather than loading into program memory) locks it to that device the
  same way it does today — job folders don't change that behavior, they
  only change how many clicks it takes to get to the file.

## Fusion post configuration

Point the post's output path at the share root (`\\pi\cnc\`, or whatever UNC
path the SMB share resolves to for the workstation running Fusion) — **not**
at a job-specific subfolder. Posting into the root is what makes the file
show up as "loose at the root" the instant it lands, which is exactly what
the auto-bucket watcher is waiting for: it files itself into
`J000020 - F-BRACKET-REV-C/` within a few seconds of the post completing,
with no operator action and no per-job path to remember or get wrong in the
post's output dialog.
