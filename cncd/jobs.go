// Job-bucketed root layout: one folder per job directly under the
// served directory, named from the program identity header
// (docs/PROGRAM_IDENTITY.md), so the Haas control's LIST PROGRAM
// screen shows each job without an extra click into a top-level
// folder. See docs/JOB_FOLDERS.md for the operator-facing story.
//
// Three ways a file ends up in its job folder:
//   - POST /api/jobs/file moves one caller-named loose root file.
//   - The auto-bucket watcher (RunAutoBucketWatcher) polls the root
//     every few seconds and moves any loose file that carries a GMW
//     header once it's been stable long enough that a post is
//     unlikely to still be writing it.
//   - Nothing else, ever: files already inside a job folder — or any
//     other folder — are never touched, and a move is always a
//     rename (no copy+delete), so the "never delete" rule holds by
//     construction.
//
// A file with no GMW header has no job to file it under, so it stays
// exactly where it was dropped — the root is the inbox, not a
// separate "_INBOX" folder (that was the whole point of this
// feature: fewer folders to click through, not more).
package cncd

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/filebrowser/filebrowser/v2/cnc"
)

// maxJobFolderNameLen bounds the sanitised folder name. Chosen well
// under FAT32's 255-char LFN ceiling and short enough to read
// comfortably on a Haas control's single-line file list.
const maxJobFolderNameLen = 32

// jobFolderDisallowed matches every rune NOT in the safe set: A-Z,
// 0-9, space, dot, dash, underscore. Restricting to this set keeps
// job folder names legible on FAT32 (the USB-gadget image), SMB1 (the
// Haas's guest mount), and the Haas control's own file list, which is
// pickier about punctuation than a modern OS.
var jobFolderDisallowed = regexp.MustCompile(`[^A-Z0-9 ._-]`)

var (
	collapseUnderscores = regexp.MustCompile(`_{2,}`)
	collapseSpaces      = regexp.MustCompile(` {2,}`)
)

// sanitizeJobToken upper-cases s and replaces every character outside
// the safe set with "_", then collapses any run of underscores or
// spaces that produced (so "F/BRACKET" doesn't become "F__BRACKET")
// and trims stray separators off both ends.
func sanitizeJobToken(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	s = jobFolderDisallowed.ReplaceAllString(s, "_")
	s = collapseUnderscores.ReplaceAllString(s, "_")
	s = collapseSpaces.ReplaceAllString(s, " ")
	s = strings.Trim(s, " _.-")
	return s
}

// JobFolderName builds the sanitised, length-bounded folder name for
// a job/part pair: "J000020 - F-BRACKET-REV-C". part is optional —
// when it sanitises to empty (missing, or nothing but disallowed
// characters), the folder is named from job alone ("J000020"), which
// is also the fallback docs/JOB_FOLDERS.md and this task describe.
// Returns "" when job itself sanitises to empty — a header with no
// (or an unparseable) GMW-JOB has nothing to bucket by, so the caller
// should leave the file where it is rather than invent a name.
func JobFolderName(job, part string) string {
	j := sanitizeJobToken(job)
	if j == "" {
		return ""
	}
	name := j
	if p := sanitizeJobToken(part); p != "" {
		name = j + " - " + p
	}
	if len(name) > maxJobFolderNameLen {
		name = strings.TrimRight(name[:maxJobFolderNameLen], " -_.")
		if name == "" {
			// Truncation ate everything meaningful (pathological:
			// job id alone is already >32 chars of separators) —
			// fall back to the untruncated job token, hard-cut.
			name = j
			if len(name) > maxJobFolderNameLen {
				name = name[:maxJobFolderNameLen]
			}
		}
	}
	return name
}

// jobsFolderNameRe recognizes JobFolderName's own "<JOB> - <PART>"
// shape well enough to recover job/part from a directory name without
// re-reading any file — the fast path buildJobsList tries first.
var jobsFolderNameSep = " - "

// parseJobFolderName best-effort splits name on JobFolderName's own
// " - " separator. ok is false when the separator isn't present, in
// which case job is the whole name (still a reasonable guess for a
// job-only folder, e.g. "J000020") and part is empty.
func parseJobFolderName(name string) (job, part string, ok bool) {
	if idx := strings.Index(name, jobsFolderNameSep); idx >= 0 {
		return strings.TrimSpace(name[:idx]), strings.TrimSpace(name[idx+len(jobsFolderNameSep):]), true
	}
	return name, "", false
}

// sidecarSuffix mirrors cncd/mcp/list_programs.go's constant of the
// same name (and cnc.SidecarPath's convention) — duplicated as a
// literal rather than imported/exported since it's a two-line rule,
// not a shared abstraction worth a new seam.
const sidecarSuffix = ".gmw.json"

// identityScanMaxBytes bounds how large a file this package will read
// looking for a GMW header, mirroring
// cncd/mcp/list_programs.go's listProgramsMaxScanBytes for the same
// reason: real NC programs are far smaller, and skipping the read on
// an oversized file keeps a stray large file from being loaded whole
// into memory on the Pi.
const identityScanMaxBytes = 8 * 1024 * 1024

// scanIdentity reads path (if it's within identityScanMaxBytes) and
// returns its parsed GMW header, or nil when the file is too large,
// unreadable, or carries no header at all.
func scanIdentity(path string) *cnc.Identity {
	info, err := os.Stat(path)
	if err != nil || info.Size() == 0 || info.Size() > identityScanMaxBytes {
		return nil
	}
	buf, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	id, ok := cnc.ParseIdentity(buf)
	if !ok {
		return nil
	}
	return id
}

// ── GET /api/jobs ───────────────────────────────────────────────────

// JobSummary is one row of GET /api/jobs — a top-level folder under
// the served root.
type JobSummary struct {
	Name          string               `json:"name"`
	Job           string               `json:"job,omitempty"`
	Operation     string               `json:"operation,omitempty"`
	Part          string               `json:"part,omitempty"`
	ProgramCount  int                  `json:"program_count"`
	SidecarCount  int                  `json:"sidecar_count"`
	NewestModTime time.Time            `json:"newest_mod_time,omitempty"`
	// SHAStatus rolls every identified program's SHAStatus up to one
	// value: "mismatch" if any program's body drifted from its
	// header, else "unstamped" if any program is still PENDING, else
	// "match" when every identified program's hash checks out. Empty
	// when the folder holds no identified programs at all (e.g. it
	// was created ahead of time via POST /api/jobs and is still
	// empty, or holds only un-annotated files).
	SHAStatus string `json:"sha_status,omitempty"`
	// LatestRun is the most recent cnc/job_history.go entry across
	// every configured machine whose FilePath falls under this
	// folder, matched by path prefix. nil when nothing has ever run
	// from this folder.
	LatestRun *cnc.JobHistoryEntry `json:"latest_run,omitempty"`
}

// UnfiledEntry is one loose file sitting directly at the served root
// — not yet (or never going to be, if it carries no header) filed
// into a job folder.
type UnfiledEntry struct {
	Name        string    `json:"name"`
	Size        int64     `json:"size"`
	ModTime     time.Time `json:"mod_time"`
	HasIdentity bool      `json:"has_identity"`
	Job         string    `json:"job,omitempty"`
	Part        string    `json:"part,omitempty"`
}

// JobsListResult is GET /api/jobs' response body.
type JobsListResult struct {
	Jobs    []JobSummary   `json:"jobs"`
	Unfiled []UnfiledEntry `json:"unfiled"`
}

// buildJobsList scans the served root's top-level entries: every
// directory (other than the tool-table dump dir) becomes a
// JobSummary, every loose file becomes an UnfiledEntry.
func buildJobsList(d Deps) (*JobsListResult, error) {
	entries, err := os.ReadDir(d.Root)
	if err != nil {
		return nil, err
	}

	res := &JobsListResult{Jobs: []JobSummary{}, Unfiled: []UnfiledEntry{}}
	history := loadAllJobHistory(d)

	for _, e := range entries {
		if e.IsDir() {
			if e.Name() == toolTableShareDir {
				continue
			}
			summary, err := summarizeJobFolder(d.Root, e.Name(), history)
			if err != nil {
				continue // unreadable folder — best-effort listing
			}
			res.Jobs = append(res.Jobs, *summary)
			continue
		}
		if strings.HasSuffix(e.Name(), sidecarSuffix) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		entry := UnfiledEntry{Name: e.Name(), Size: info.Size(), ModTime: info.ModTime()}
		if id := scanIdentity(filepath.Join(d.Root, e.Name())); id != nil {
			entry.HasIdentity = true
			entry.Job = id.Job
			entry.Part = id.Part
		}
		res.Unfiled = append(res.Unfiled, entry)
	}

	sort.Slice(res.Jobs, func(i, j int) bool { return res.Jobs[i].Name < res.Jobs[j].Name })
	sort.Slice(res.Unfiled, func(i, j int) bool { return res.Unfiled[i].Name < res.Unfiled[j].Name })
	return res, nil
}

// summarizeJobFolder builds one JobSummary for the folder dirName
// directly under root.
func summarizeJobFolder(root, dirName string, history []cnc.JobHistoryEntry) (*JobSummary, error) {
	abs := filepath.Join(root, dirName)
	summary := &JobSummary{Name: dirName}
	summary.Job, summary.Part, _ = parseJobFolderName(dirName)

	var (
		firstIdentity *cnc.Identity
		sawMismatch   bool
		sawUnstamped  bool
		sawMatch      bool
	)

	err := filepath.WalkDir(abs, func(p string, de os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if de.IsDir() {
			if p != abs && de.Name() == toolTableShareDir {
				return filepath.SkipDir
			}
			return nil
		}
		info, ferr := de.Info()
		if ferr != nil {
			return nil //nolint:nilerr // best-effort summary; skip unreadable entries
		}
		if info.ModTime().After(summary.NewestModTime) {
			summary.NewestModTime = info.ModTime()
		}
		if strings.HasSuffix(de.Name(), sidecarSuffix) {
			summary.SidecarCount++
			return nil
		}
		summary.ProgramCount++
		if id := scanIdentity(p); id != nil {
			if firstIdentity == nil {
				firstIdentity = id
			}
			switch id.SHAStatus {
			case cnc.SHAMismatch:
				sawMismatch = true
			case cnc.SHAUnstamped:
				sawUnstamped = true
			case cnc.SHAMatchStatus:
				sawMatch = true
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	if firstIdentity != nil {
		// A real identity found inside the folder is ground truth
		// over the folder-name heuristic — override.
		summary.Job = firstIdentity.Job
		summary.Operation = firstIdentity.Operation
		summary.Part = firstIdentity.Part
	}

	switch {
	case sawMismatch:
		summary.SHAStatus = cnc.SHAMismatch
	case sawUnstamped:
		summary.SHAStatus = cnc.SHAUnstamped
	case sawMatch:
		summary.SHAStatus = cnc.SHAMatchStatus
	}

	summary.LatestRun = latestRunForFolder(history, dirName)
	return summary, nil
}

// loadAllJobHistory reads every configured machine's job history
// (cnc.ReadJobHistory) into one slice. Best-effort: a machine with no
// history file yet contributes nothing, not an error.
func loadAllJobHistory(d Deps) []cnc.JobHistoryEntry {
	cfg := d.Config.Snapshot()
	var out []cnc.JobHistoryEntry
	seen := map[string]bool{}
	ids := make([]string, 0, len(cfg.Machines))
	for _, m := range cfg.Machines {
		if m.ID == "" || seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		ids = append(ids, m.ID)
	}
	if len(ids) == 0 {
		ids = []string{""} // "default" — see historyPathFor
	}
	for _, id := range ids {
		entries, err := cnc.ReadJobHistory(id, 0)
		if err != nil {
			continue
		}
		out = append(out, entries...)
	}
	return out
}

// latestRunForFolder returns the most recent history entry whose
// FilePath falls under dirName (matched by path prefix, tolerant of
// a leading "/" either way — see cnc/streamer.go's displayPath doc).
// history is assumed already newest-first per machine (ReadJobHistory's
// contract); since it may interleave several machines, the whole
// slice is scanned rather than assuming global order.
func latestRunForFolder(history []cnc.JobHistoryEntry, dirName string) *cnc.JobHistoryEntry {
	prefix := strings.Trim(dirName, "/") + "/"
	var best *cnc.JobHistoryEntry
	for i := range history {
		e := history[i]
		if !strings.HasPrefix(strings.TrimPrefix(e.FilePath, "/"), prefix) {
			continue
		}
		if best == nil || e.StartedAt.After(best.StartedAt) {
			ec := e
			best = &ec
		}
	}
	return best
}

// ── POST /api/jobs ──────────────────────────────────────────────────

// createJobFolder resolves the folder name for job/part and creates
// it under root if it doesn't already exist. Idempotent: an existing
// folder is not an error. Returns the resolved name and whether this
// call actually created it.
func createJobFolder(root, job, part string) (name string, created bool, err error) {
	name = JobFolderName(job, part)
	if name == "" {
		return "", false, fmt.Errorf("cncd: job %q has no usable characters for a folder name", job)
	}
	abs := filepath.Join(root, name)
	if info, statErr := os.Stat(abs); statErr == nil {
		if !info.IsDir() {
			return "", false, fmt.Errorf("cncd: %q already exists and is not a directory", name)
		}
		return name, false, nil
	}
	if err := os.Mkdir(abs, 0o755); err != nil {
		return "", false, err
	}
	return name, true, nil
}

// ── POST /api/jobs/file ─────────────────────────────────────────────

// fileLooseRootFile moves the single loose root file at absPath (its
// parent directory must be root itself — enforced by the caller) into
// its job folder, based on the GMW header it carries, moving its
// .gmw.json sidecar alongside if one exists. Creates the destination
// folder if needed. Returns the moved-to path relative to root.
//
// Never called for anything already inside a subfolder — "never move
// files inside existing folders" is enforced by callers only ever
// handing this a root-level path, not by any check in here.
func fileLooseRootFile(root, absPath string) (destRel string, err error) {
	id := scanIdentity(absPath)
	if id == nil || id.Job == "" {
		return "", fmt.Errorf("cncd: %s carries no GMW-JOB header — nothing to file it under", filepath.Base(absPath))
	}
	folder := JobFolderName(id.Job, id.Part)
	if folder == "" {
		return "", fmt.Errorf("cncd: %s's job id sanitises to nothing usable", filepath.Base(absPath))
	}
	destDir := filepath.Join(root, folder)
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", err
	}
	base := filepath.Base(absPath)
	destPath := filepath.Join(destDir, base)
	if _, err := os.Stat(destPath); err == nil {
		return "", fmt.Errorf("cncd: %s already exists in %s", base, folder)
	}
	if err := os.Rename(absPath, destPath); err != nil {
		return "", err
	}
	// Sidecar rides along, best-effort — a missing sidecar is normal
	// (not every program has run through the identity post yet).
	srcSidecar := cnc.SidecarPath(absPath)
	if _, statErr := os.Stat(srcSidecar); statErr == nil {
		_ = os.Rename(srcSidecar, cnc.SidecarPath(destPath))
	}
	return filepath.ToSlash(filepath.Join(folder, base)), nil
}

// ── Auto-bucket watcher ─────────────────────────────────────────────

// bucketStabilityWindow is how long a loose root file's mtime must be
// unchanged before the watcher will move it — long enough that a post
// (or an upload still streaming in over PUT /api/files) is unlikely
// to still be writing it, short enough that an operator doesn't sit
// looking at an un-bucketed file for long.
const bucketStabilityWindow = 2 * time.Second

// bucketStableRootFiles is one pass of the auto-bucket watcher: scan
// the served root's direct children (never recursing into existing
// folders — see the package doc's "never move" rule) and file every
// stable, identity-bearing loose file into its job folder. Returns
// the root-relative destinations of everything it moved this pass.
// Best-effort per file: one file's error (e.g. a name collision at
// the destination) doesn't stop the rest of the pass.
func bucketStableRootFiles(d Deps) []string {
	entries, err := os.ReadDir(d.Root)
	if err != nil {
		return nil
	}
	now := time.Now()
	var moved []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, sidecarSuffix) {
			continue // rides along with its NC file, never moved on its own
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) < bucketStabilityWindow {
			continue // may still be mid-write
		}
		dest, err := fileLooseRootFile(d.Root, filepath.Join(d.Root, name))
		if err != nil {
			continue // no header, name collision, etc — leave it for a human
		}
		moved = append(moved, dest)
	}
	return moved
}

// defaultAutoBucketInterval is the watcher's poll cadence when the
// caller doesn't override it. Matches docs/JOB_FOLDERS.md's "every
// 5 seconds."
const defaultAutoBucketInterval = 5 * time.Second

// ShouldAutoBucket reports whether the auto-bucket watcher should be
// running for the current config — false when RootIsJobs is
// explicitly off, or AutoBucket is explicitly off. cmd/cncd checks
// this once at startup before spawning RunAutoBucketWatcher; the
// watcher itself doesn't re-check per tick, so toggling either knob
// requires a restart, same as every other cncd config change today.
func ShouldAutoBucket(d Deps) bool {
	return d.Config.Snapshot().Jobs.AutoBucketResolved()
}

// RunAutoBucketWatcher polls the served root every interval (<= 0
// uses defaultAutoBucketInterval) and files any stable, GMW-header-
// bearing loose root file into its job folder, until ctx is done.
// Intended to run in its own goroutine for the life of the daemon
// (see cmd/cncd/main.go) — errors from an individual pass are logged,
// never fatal, since a bad pass (e.g. a transient permission error)
// shouldn't take the whole daemon down.
func RunAutoBucketWatcher(ctx context.Context, d Deps, interval time.Duration) {
	if interval <= 0 {
		interval = defaultAutoBucketInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if moved := bucketStableRootFiles(d); len(moved) > 0 {
				log.Printf("cncd: jobs auto-bucket: filed %v", moved)
			}
		}
	}
}
