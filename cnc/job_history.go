package cnc

// Per-job history log — one JSONL line per completed streaming job.
// Captures start / end / duration / status so the dashboard can render
// utilization stats without re-deriving anything from the streamer
// event log (which is in-memory only).
//
// Lives next to the recovery marker under <state>/job_history.jsonl
// so it survives Pi reboots. Append-only; the reader tails the file
// rather than loading the whole thing. Real shops will see ~5-50
// rows per day, so a year of history is a few hundred KB — far below
// any need for rotation.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// JobHistoryEntry is one row in the history log. Mirrors the streamer's
// job struct + adds an exit-status field. ErrorMsg is set on streamer
// errors (dial fail, file open fail, write fail); LineFinal == LineTotal
// implies a clean completion.
type JobHistoryEntry struct {
	JobID       string    `json:"job_id"`
	MachineID   string    `json:"machine_id"`
	FilePath    string    `json:"file_path"`
	Method      string    `json:"method,omitempty"` // mem / dnc
	StartedAt   time.Time `json:"started_at"`
	EndedAt     time.Time `json:"ended_at"`
	DurationMs  int64     `json:"duration_ms"`
	LineTotal   int       `json:"line_total"`
	LineFinal   int64     `json:"line_final"`
	// Status: "completed" | "stopped" | "error". Picked by the streamer
	// at exit: ctx.Err() != nil → "stopped", lastError set → "error",
	// otherwise → "completed".
	Status   string `json:"status"`
	ErrorMsg string `json:"error_msg,omitempty"`
}

// historyPathFor lives next to the active-job marker under the same
// CNC_STATE_DIR. Per-machine so two parallel machines don't interleave.
func historyPathFor(machineID string) string {
	if machineID == "" {
		machineID = "default"
	}
	return filepath.Join(markerStateDir(), "job_history_"+machineID+".jsonl")
}

// AppendJobHistory writes one entry to the per-machine JSONL log.
// Best-effort: failures are returned but the caller (streamer defer)
// just logs them. We never block job termination on the log write.
func AppendJobHistory(machineID string, e JobHistoryEntry) error {
	p := historyPathFor(machineID)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	buf, err := json.Marshal(&e)
	if err != nil {
		return err
	}
	buf = append(buf, '\n')
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, werr := f.Write(buf)
	return werr
}

// ReadJobHistory returns the most recent `limit` entries for a machine,
// newest first. limit <= 0 reads everything. Missing file is not an
// error — fresh installs return an empty slice.
func ReadJobHistory(machineID string, limit int) ([]JobHistoryEntry, error) {
	p := historyPathFor(machineID)
	f, err := os.Open(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []JobHistoryEntry{}, nil
		}
		return nil, err
	}
	defer f.Close()

	// Linear scan is fine — even a year of history is a few hundred KB
	// and the dashboard only fetches this on /analytics page mount.
	// Sorting by StartedAt-desc at the end handles any out-of-order
	// writes (concurrent jobs across machines write to separate files,
	// so realistically this is already in append order).
	out := []JobHistoryEntry{}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var e JobHistoryEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			// Malformed line — skip rather than abort the whole read.
			// A power-fail mid-write can produce a partial last line.
			continue
		}
		out = append(out, e)
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].StartedAt.After(out[j].StartedAt)
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// JobStats is a windowed aggregate of the history log. Counts each
// terminal status, plus total run time (only "completed" + "stopped"
// rows — "error" jobs may have run 0 seconds before a dial failure).
type JobStats struct {
	MachineID         string             `json:"machine_id"`
	WindowDays        int                `json:"window_days"`
	WindowStart       time.Time          `json:"window_start"`
	TotalJobs         int                `json:"total_jobs"`
	CompletedJobs     int                `json:"completed_jobs"`
	StoppedJobs       int                `json:"stopped_jobs"`
	ErroredJobs       int                `json:"errored_jobs"`
	TotalRunSeconds   int64              `json:"total_run_seconds"`
	AvgRunSeconds     int64              `json:"avg_run_seconds"`
	LongestRunSeconds int64              `json:"longest_run_seconds"`
	// LastJob is the single most recent row in the window (or nil).
	LastJob *JobHistoryEntry `json:"last_job,omitempty"`
	// TopFiles is the top-5 most-run files in the window with their
	// counts + accumulated runtime. Operators use this to identify
	// the dominant programs on a given machine.
	TopFiles []JobFileStat `json:"top_files,omitempty"`
}

// JobFileStat is one row in JobStats.TopFiles.
type JobFileStat struct {
	FilePath   string `json:"file_path"`
	Runs       int    `json:"runs"`
	RunSeconds int64  `json:"run_seconds"`
}

// ComputeJobStats reads the full history and returns aggregates for
// the last `windowDays` (24 h × N). windowDays <= 0 means "all time."
// Pure data — no I/O beyond the initial read.
func ComputeJobStats(machineID string, windowDays int) (*JobStats, error) {
	entries, err := ReadJobHistory(machineID, 0)
	if err != nil {
		return nil, err
	}
	out := &JobStats{
		MachineID:  machineID,
		WindowDays: windowDays,
	}
	var cutoff time.Time
	if windowDays > 0 {
		cutoff = time.Now().UTC().Add(-time.Duration(windowDays) * 24 * time.Hour)
		out.WindowStart = cutoff
	}
	fileAgg := map[string]*JobFileStat{}
	for _, e := range entries {
		if windowDays > 0 && e.StartedAt.Before(cutoff) {
			continue
		}
		out.TotalJobs++
		switch e.Status {
		case "completed":
			out.CompletedJobs++
		case "stopped":
			out.StoppedJobs++
		case "error":
			out.ErroredJobs++
		}
		// Per-file aggregation. Errored jobs still count toward "runs"
		// (the operator did press send), but their seconds are usually 0.
		fa := fileAgg[e.FilePath]
		if fa == nil {
			fa = &JobFileStat{FilePath: e.FilePath}
			fileAgg[e.FilePath] = fa
		}
		fa.Runs++
		if e.Status != "error" {
			secs := e.DurationMs / 1000
			fa.RunSeconds += secs
			out.TotalRunSeconds += secs
			if secs > out.LongestRunSeconds {
				out.LongestRunSeconds = secs
			}
		}
		if out.LastJob == nil {
			// entries are newest-first from ReadJobHistory, so the first
			// one we see in-window is the most recent.
			ec := e
			out.LastJob = &ec
		}
	}
	if out.CompletedJobs+out.StoppedJobs > 0 {
		out.AvgRunSeconds = out.TotalRunSeconds / int64(out.CompletedJobs+out.StoppedJobs)
	}
	// Top-5 files by run count, tie-broken by run-seconds desc.
	flat := make([]JobFileStat, 0, len(fileAgg))
	for _, fa := range fileAgg {
		flat = append(flat, *fa)
	}
	sort.Slice(flat, func(i, j int) bool {
		if flat[i].Runs != flat[j].Runs {
			return flat[i].Runs > flat[j].Runs
		}
		return flat[i].RunSeconds > flat[j].RunSeconds
	})
	if len(flat) > 5 {
		flat = flat[:5]
	}
	out.TopFiles = flat
	return out, nil
}

// recordJobHistory is the streamer's hook — called from the run()
// defer block. ctx carries the streamer's cancel state so we can
// distinguish operator-stop ("stopped") from a controller error
// ("error") from a clean EOF ("completed"). lastError is the
// streamer's last recorded write/dial failure (empty when clean).
func recordJobHistory(ctx context.Context, machineID string, j *job, lastError string) {
	if j == nil {
		return
	}
	now := time.Now().UTC()
	status := "completed"
	if lastError != "" {
		status = "error"
	} else if ctx != nil && ctx.Err() != nil {
		// Distinguish operator stop from natural completion. Context
		// gets cancelled by Stop(); a clean run ends with the run loop
		// returning normally before the deferred cancel fires.
		status = "stopped"
	}
	e := JobHistoryEntry{
		JobID:      j.id,
		MachineID:  machineID,
		FilePath:   j.displayPath,
		Method:     string(j.method),
		StartedAt:  j.startedAt.UTC(),
		EndedAt:    now,
		DurationMs: now.Sub(j.startedAt).Milliseconds(),
		LineTotal:  j.lineTotal,
		LineFinal:  j.lineCurrent.Load(),
		Status:     status,
		ErrorMsg:   lastError,
	}
	_ = AppendJobHistory(machineID, e)
}
