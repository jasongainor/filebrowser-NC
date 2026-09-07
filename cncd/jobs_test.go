package cncd

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/filebrowser/filebrowser/v2/cnc"
	"github.com/filebrowser/filebrowser/v2/settings"
)

// identityNC mirrors cncd/mcp/list_programs_test.go's fixture — a
// well-formed GMW header (SHA left PENDING, i.e. "unstamped").
const identityNC = "(GMW-ID V1)\n(GMW-JOB J000020 OP10)\n(GMW-PART F-BRACKET-REV-C)\n" +
	"(GMW-POST HAAS-NGC V1.0.3)\n(GMW-POSTED 2026-09-07T14:22:00Z)\n(GMW-TOOLS 1)\n" +
	"(GMW-SHA PENDING)\nO0001\nT5 M06\nM30\n"

func plainNC() string { return "%\nO0001\nT5 M06\nM30\n%\n" }

// ── JobFolderName ────────────────────────────────────────────────────

func TestJobFolderName(t *testing.T) {
	cases := []struct {
		name, job, part, want string
	}{
		{"job + part, space-dash-space", "J000020", "F-BRACKET-REV-C", "J000020 - F-BRACKET-REV-C"},
		{"lowercase gets upper-cased", "j000020", "bracket", "J000020 - BRACKET"},
		{"no part falls back to job alone", "J000020", "", "J000020"},
		{"whitespace-only part treated as empty", "J000020", "   ", "J000020"},
		{"disallowed characters become underscore", "J000020", "F/BRACKET:REV*C", "J000020 - F_BRACKET_REV_C"},
		{"repeated disallowed chars collapse to one underscore", "J000020", "F//BRACKET", "J000020 - F_BRACKET"},
		{"parens stripped", "J000020", "PART(REV C)", "J000020 - PART_REV C"},
		{"digits and dots allowed", "J1", "REV.2", "J1 - REV.2"},
		{"truncated to 32 chars", "J000020", "A-VERY-LONG-PART-NAME-THAT-WONT-FIT", "J000020 - A-VERY-LONG-PART-NAME"},
		{"empty job yields empty name", "", "F-BRACKET-REV-C", ""},
		{"job that sanitises to empty yields empty name", "///", "F-BRACKET-REV-C", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := JobFolderName(c.job, c.part)
			if got != c.want {
				t.Errorf("JobFolderName(%q, %q) = %q, want %q", c.job, c.part, got, c.want)
			}
			if len(got) > maxJobFolderNameLen {
				t.Errorf("JobFolderName(%q, %q) = %q, length %d exceeds max %d", c.job, c.part, got, len(got), maxJobFolderNameLen)
			}
		})
	}
}

// ── GET /api/jobs (buildJobsList) ───────────────────────────────────

func TestBuildJobsList_SeededRoot(t *testing.T) {
	deps, _ := newTestDeps(t, func(c *settings.Cnc) {
		c.Machines = []settings.Machine{{ID: "m1", Name: "Machine 1"}}
	})

	// A job folder our own naming convention would produce, holding
	// one identified program and its sidecar.
	jobDir := filepath.Join(deps.Root, "J000020 - F-BRACKET-REV-C")
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, jobDir, "op10.nc", identityNC)
	writeFile(t, jobDir, "op10.nc.gmw.json", `{"schema_version":1,"job":"J000020","tools":[]}`)

	// A hand-created folder with no header inside — falls back to
	// the folder-name heuristic.
	miscDir := filepath.Join(deps.Root, "MISC")
	if err := os.MkdirAll(miscDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, miscDir, "scrap.nc", plainNC())

	// The tool-table dump directory must never show up as a job.
	if err := os.MkdirAll(filepath.Join(deps.Root, toolTableShareDir, "m1"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, filepath.Join(deps.Root, toolTableShareDir, "m1"), "dump.json", `{}`)

	// A loose file at the root — unfiled.
	writeFile(t, deps.Root, "loose.nc", plainNC())
	writeFile(t, deps.Root, "loose-identified.nc", identityNC)

	result, err := buildJobsList(deps)
	if err != nil {
		t.Fatalf("buildJobsList: %v", err)
	}

	if len(result.Jobs) != 2 {
		t.Fatalf("got %d job folders, want 2 (J000020 - F-BRACKET-REV-C, MISC): %+v", len(result.Jobs), result.Jobs)
	}
	byName := map[string]JobSummary{}
	for _, j := range result.Jobs {
		byName[j.Name] = j
	}

	job := byName["J000020 - F-BRACKET-REV-C"]
	if job.Job != "J000020" || job.Operation != "OP10" || job.Part != "F-BRACKET-REV-C" {
		t.Errorf("job/op/part = %q/%q/%q, want J000020/OP10/F-BRACKET-REV-C", job.Job, job.Operation, job.Part)
	}
	if job.ProgramCount != 1 {
		t.Errorf("ProgramCount = %d, want 1", job.ProgramCount)
	}
	if job.SidecarCount != 1 {
		t.Errorf("SidecarCount = %d, want 1", job.SidecarCount)
	}
	if job.SHAStatus != cnc.SHAUnstamped {
		t.Errorf("SHAStatus = %q, want %q", job.SHAStatus, cnc.SHAUnstamped)
	}
	if job.NewestModTime.IsZero() {
		t.Errorf("NewestModTime not set")
	}

	misc := byName["MISC"]
	if misc.Job != "MISC" || misc.Part != "" {
		t.Errorf("MISC folder-name fallback = job=%q part=%q, want job=MISC part=\"\"", misc.Job, misc.Part)
	}
	if misc.ProgramCount != 1 {
		t.Errorf("MISC ProgramCount = %d, want 1", misc.ProgramCount)
	}
	if misc.SHAStatus != "" {
		t.Errorf("MISC SHAStatus = %q, want empty (no identified programs)", misc.SHAStatus)
	}

	if len(result.Unfiled) != 2 {
		t.Fatalf("got %d unfiled entries, want 2: %+v", len(result.Unfiled), result.Unfiled)
	}
	byUnfiled := map[string]UnfiledEntry{}
	for _, u := range result.Unfiled {
		byUnfiled[u.Name] = u
	}
	if e := byUnfiled["loose.nc"]; e.HasIdentity {
		t.Errorf("loose.nc should have no identity")
	}
	if e := byUnfiled["loose-identified.nc"]; !e.HasIdentity || e.Job != "J000020" {
		t.Errorf("loose-identified.nc = %+v, want HasIdentity with job J000020", e)
	}
}

func TestBuildJobsList_LatestRunMatchedByPathPrefix(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CNC_STATE_DIR", dir)

	deps, _ := newTestDeps(t, func(c *settings.Cnc) {
		c.Machines = []settings.Machine{{ID: "m1", Name: "Machine 1"}}
	})

	jobDir := filepath.Join(deps.Root, "J000020 - F-BRACKET-REV-C")
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, jobDir, "op10.nc", identityNC)

	older := cnc.JobHistoryEntry{
		JobID: "a", MachineID: "m1", FilePath: "/J000020 - F-BRACKET-REV-C/op10.nc",
		StartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Status: "completed",
	}
	newer := cnc.JobHistoryEntry{
		JobID: "b", MachineID: "m1", FilePath: "/J000020 - F-BRACKET-REV-C/op10.nc",
		StartedAt: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC), Status: "completed",
	}
	elsewhere := cnc.JobHistoryEntry{
		JobID: "c", MachineID: "m1", FilePath: "/MISC/scrap.nc",
		StartedAt: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), Status: "completed",
	}
	for _, e := range []cnc.JobHistoryEntry{older, newer, elsewhere} {
		if err := cnc.AppendJobHistory("m1", e); err != nil {
			t.Fatalf("AppendJobHistory: %v", err)
		}
	}

	result, err := buildJobsList(deps)
	if err != nil {
		t.Fatalf("buildJobsList: %v", err)
	}
	var job *JobSummary
	for i := range result.Jobs {
		if result.Jobs[i].Name == "J000020 - F-BRACKET-REV-C" {
			job = &result.Jobs[i]
		}
	}
	if job == nil {
		t.Fatalf("job folder not found in %+v", result.Jobs)
	}
	if job.LatestRun == nil || job.LatestRun.JobID != "b" {
		t.Fatalf("LatestRun = %+v, want the newer entry (JobID b)", job.LatestRun)
	}
}

// ── POST /api/jobs (createJobFolder) ────────────────────────────────

func TestCreateJobFolder_Idempotent(t *testing.T) {
	root := t.TempDir()

	name, created, err := createJobFolder(root, "J000020", "F-BRACKET-REV-C")
	if err != nil {
		t.Fatalf("createJobFolder: %v", err)
	}
	if name != "J000020 - F-BRACKET-REV-C" {
		t.Errorf("name = %q", name)
	}
	if !created {
		t.Errorf("expected created=true on first call")
	}
	if info, statErr := os.Stat(filepath.Join(root, name)); statErr != nil || !info.IsDir() {
		t.Fatalf("expected folder to exist: %v", statErr)
	}

	name2, created2, err := createJobFolder(root, "J000020", "F-BRACKET-REV-C")
	if err != nil {
		t.Fatalf("createJobFolder (again): %v", err)
	}
	if name2 != name {
		t.Errorf("resolved name changed across calls: %q vs %q", name2, name)
	}
	if created2 {
		t.Errorf("expected created=false on second call (already exists)")
	}
}

func TestCreateJobFolder_EmptyJobRejected(t *testing.T) {
	root := t.TempDir()
	if _, _, err := createJobFolder(root, "", "PART"); err == nil {
		t.Fatalf("expected an error for an empty job id")
	}
}

// ── POST /api/jobs/file (fileLooseRootFile) ─────────────────────────

func TestFileLooseRootFile_MovesSidecar(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "op10.nc", identityNC)
	writeFile(t, root, "op10.nc.gmw.json", `{"schema_version":1,"job":"J000020","tools":[]}`)

	dest, err := fileLooseRootFile(root, filepath.Join(root, "op10.nc"))
	if err != nil {
		t.Fatalf("fileLooseRootFile: %v", err)
	}
	if dest != "J000020 - F-BRACKET-REV-C/op10.nc" {
		t.Errorf("dest = %q", dest)
	}
	if _, err := os.Stat(filepath.Join(root, "op10.nc")); !os.IsNotExist(err) {
		t.Errorf("expected op10.nc to be gone from root")
	}
	if _, err := os.Stat(filepath.Join(root, dest)); err != nil {
		t.Errorf("expected %s to exist: %v", dest, err)
	}
	sidecarDest := filepath.Join(root, "J000020 - F-BRACKET-REV-C", "op10.nc.gmw.json")
	if _, err := os.Stat(sidecarDest); err != nil {
		t.Errorf("expected sidecar to move alongside: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "op10.nc.gmw.json")); !os.IsNotExist(err) {
		t.Errorf("expected sidecar to be gone from root")
	}
}

func TestFileLooseRootFile_NoHeaderRejected(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "plain.nc", plainNC())

	if _, err := fileLooseRootFile(root, filepath.Join(root, "plain.nc")); err == nil {
		t.Fatalf("expected an error for a file with no GMW header")
	}
	if _, err := os.Stat(filepath.Join(root, "plain.nc")); err != nil {
		t.Errorf("plain.nc should be untouched: %v", err)
	}
}

// ── Auto-bucket watcher ──────────────────────────────────────────────

func TestBucketStableRootFiles_MovesStableLeavesFresh(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "stable.nc", identityNC)
	writeFile(t, root, "fresh.nc", identityNC)

	// Back-date "stable.nc" past the stability window; leave
	// "fresh.nc" at its just-written mtime (well within the window).
	old := time.Now().Add(-10 * time.Second)
	if err := os.Chtimes(filepath.Join(root, "stable.nc"), old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	deps, _ := newTestDepsWithRoot(t, root, nil)
	moved := bucketStableRootFiles(deps)

	if len(moved) != 1 || moved[0] != "J000020 - F-BRACKET-REV-C/stable.nc" {
		t.Fatalf("moved = %v, want exactly stable.nc filed", moved)
	}
	if _, err := os.Stat(filepath.Join(root, "stable.nc")); !os.IsNotExist(err) {
		t.Errorf("stable.nc should have been moved out of root")
	}
	if _, err := os.Stat(filepath.Join(root, "fresh.nc")); err != nil {
		t.Errorf("fresh.nc (too recent to be stable) should still be at root: %v", err)
	}
}

func TestBucketStableRootFiles_NeverTouchesFilesInsideFolders(t *testing.T) {
	root := t.TempDir()
	existing := filepath.Join(root, "SOME-OTHER-FOLDER")
	if err := os.MkdirAll(existing, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, existing, "already-filed.nc", identityNC)
	old := time.Now().Add(-10 * time.Second)
	if err := os.Chtimes(filepath.Join(existing, "already-filed.nc"), old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	deps, _ := newTestDepsWithRoot(t, root, nil)
	moved := bucketStableRootFiles(deps)

	if len(moved) != 0 {
		t.Fatalf("expected nothing moved (file was already inside a folder), got %v", moved)
	}
	if _, err := os.Stat(filepath.Join(existing, "already-filed.nc")); err != nil {
		t.Errorf("file should remain exactly where it was: %v", err)
	}
}

func TestShouldAutoBucket(t *testing.T) {
	trueVal, falseVal := true, false

	deps, _ := newTestDeps(t, nil)
	if !ShouldAutoBucket(deps) {
		t.Errorf("default (unconfigured) config should auto-bucket")
	}

	deps, _ = newTestDeps(t, func(c *settings.Cnc) {
		c.Jobs.RootIsJobs = &falseVal
	})
	if ShouldAutoBucket(deps) {
		t.Errorf("rootIsJobs=false should disable auto-bucket")
	}

	deps, _ = newTestDeps(t, func(c *settings.Cnc) {
		c.Jobs.AutoBucket = &falseVal
	})
	if ShouldAutoBucket(deps) {
		t.Errorf("autoBucket=false should disable auto-bucket")
	}

	deps, _ = newTestDeps(t, func(c *settings.Cnc) {
		c.Jobs.RootIsJobs = &trueVal
		c.Jobs.AutoBucket = &trueVal
	})
	if !ShouldAutoBucket(deps) {
		t.Errorf("explicit true/true should auto-bucket")
	}
}
