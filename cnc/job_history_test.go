package cnc

import (
	"os"
	"testing"
	"time"
)

// Round-trip a few entries through the JSONL log, then verify the
// reader returns them newest-first and respects the limit. Uses
// CNC_STATE_DIR to keep the test files out of the user's cache dir.
func TestAppendAndReadJobHistoryNewestFirst(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CNC_STATE_DIR", dir)

	base := time.Date(2026, 5, 18, 10, 0, 0, 0, time.UTC)
	mk := func(jobID, file string, offset time.Duration, status string) JobHistoryEntry {
		started := base.Add(offset)
		ended := started.Add(30 * time.Second)
		return JobHistoryEntry{
			JobID:      jobID,
			MachineID:  "mill-1",
			FilePath:   file,
			Method:     "mem",
			StartedAt:  started,
			EndedAt:    ended,
			DurationMs: ended.Sub(started).Milliseconds(),
			LineTotal:  100,
			LineFinal:  100,
			Status:     status,
		}
	}

	for _, e := range []JobHistoryEntry{
		mk("a", "/part1.nc", 0, "completed"),
		mk("b", "/part2.nc", 10*time.Minute, "stopped"),
		mk("c", "/part1.nc", 20*time.Minute, "completed"),
	} {
		if err := AppendJobHistory("mill-1", e); err != nil {
			t.Fatalf("append %s: %v", e.JobID, err)
		}
	}

	got, err := ReadJobHistory("mill-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(got))
	}
	if got[0].JobID != "c" {
		t.Fatalf("expected newest first (c), got %s", got[0].JobID)
	}
	if got[2].JobID != "a" {
		t.Fatalf("expected oldest last (a), got %s", got[2].JobID)
	}

	// Limit clamps to the newest N.
	got2, err := ReadJobHistory("mill-1", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got2) != 2 || got2[0].JobID != "c" || got2[1].JobID != "b" {
		t.Fatalf("limit 2 returned wrong slice: %+v", got2)
	}
}

// Stats with a 7-day window includes both in-window jobs, sums the
// duration of non-errored runs, and picks the busiest file. The
// errored row counts toward TotalJobs / Errored but not RunSeconds.
func TestComputeJobStatsWindow(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CNC_STATE_DIR", dir)

	now := time.Now().UTC()
	rows := []JobHistoryEntry{
		// 2 days ago — in 7-day window
		{JobID: "a", MachineID: "mill-1", FilePath: "/part1.nc",
			StartedAt: now.Add(-48 * time.Hour), EndedAt: now.Add(-48*time.Hour + 5*time.Minute),
			DurationMs: 300_000, Status: "completed"},
		// 1 day ago — in window — second run of part1
		{JobID: "b", MachineID: "mill-1", FilePath: "/part1.nc",
			StartedAt: now.Add(-24 * time.Hour), EndedAt: now.Add(-24*time.Hour + 4*time.Minute),
			DurationMs: 240_000, Status: "completed"},
		// 12h ago — in window — errored (counts toward total, not seconds)
		{JobID: "c", MachineID: "mill-1", FilePath: "/part2.nc",
			StartedAt: now.Add(-12 * time.Hour), EndedAt: now.Add(-12 * time.Hour),
			DurationMs: 0, Status: "error", ErrorMsg: "dial failed"},
		// 10 days ago — OUT of 7-day window
		{JobID: "d", MachineID: "mill-1", FilePath: "/part1.nc",
			StartedAt: now.Add(-10 * 24 * time.Hour), EndedAt: now.Add(-10*24*time.Hour + 2*time.Minute),
			DurationMs: 120_000, Status: "completed"},
	}
	for _, e := range rows {
		if err := AppendJobHistory("mill-1", e); err != nil {
			t.Fatal(err)
		}
	}

	st, err := ComputeJobStats("mill-1", 7)
	if err != nil {
		t.Fatal(err)
	}
	if st.TotalJobs != 3 {
		t.Fatalf("expected 3 in-window jobs, got %d", st.TotalJobs)
	}
	if st.CompletedJobs != 2 {
		t.Fatalf("expected 2 completed, got %d", st.CompletedJobs)
	}
	if st.ErroredJobs != 1 {
		t.Fatalf("expected 1 errored, got %d", st.ErroredJobs)
	}
	// 300 + 240 = 540 seconds. Errored row contributes 0.
	if st.TotalRunSeconds != 540 {
		t.Fatalf("expected 540 total run seconds, got %d", st.TotalRunSeconds)
	}
	if st.AvgRunSeconds != 270 {
		t.Fatalf("expected 270 avg run seconds, got %d", st.AvgRunSeconds)
	}
	if st.LongestRunSeconds != 300 {
		t.Fatalf("expected 300 longest run, got %d", st.LongestRunSeconds)
	}
	// Top file should be part1.nc with 2 runs.
	if len(st.TopFiles) < 1 || st.TopFiles[0].FilePath != "/part1.nc" || st.TopFiles[0].Runs != 2 {
		t.Fatalf("expected /part1.nc top with 2 runs, got %+v", st.TopFiles)
	}
}

// All-time stats (days == 0) includes the 10-day-old row.
func TestComputeJobStatsAllTime(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CNC_STATE_DIR", dir)

	now := time.Now().UTC()
	for i, age := range []time.Duration{1, 5, 30, 365} {
		_ = AppendJobHistory("mill-1", JobHistoryEntry{
			JobID:      string(rune('a' + i)),
			MachineID:  "mill-1",
			FilePath:   "/p.nc",
			StartedAt:  now.Add(-age * 24 * time.Hour),
			EndedAt:    now.Add(-age*24*time.Hour + time.Minute),
			DurationMs: 60_000,
			Status:     "completed",
		})
	}

	st, err := ComputeJobStats("mill-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if st.TotalJobs != 4 {
		t.Fatalf("expected 4 all-time, got %d", st.TotalJobs)
	}
}

// Missing history file is not an error — fresh installs return empty.
func TestReadJobHistoryMissingFile(t *testing.T) {
	t.Setenv("CNC_STATE_DIR", t.TempDir())
	got, err := ReadJobHistory("never-used", 100)
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty slice, got %+v", got)
	}
}

// Malformed trailing line (power-fail mid-write) is skipped, not
// fatal. Older clean rows still load.
func TestReadJobHistorySkipsMalformedLine(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CNC_STATE_DIR", dir)
	_ = AppendJobHistory("mill-1", JobHistoryEntry{
		JobID: "ok", MachineID: "mill-1", FilePath: "/p.nc",
		StartedAt: time.Now().UTC(), EndedAt: time.Now().UTC(),
		Status: "completed",
	})
	// Tail a torn line onto the file.
	f, err := os.OpenFile(historyPathFor("mill-1"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"job_id":"torn","machine_id":"mill-1`)
	_ = f.Close()

	got, err := ReadJobHistory("mill-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].JobID != "ok" {
		t.Fatalf("expected one clean row, got %+v", got)
	}
}
