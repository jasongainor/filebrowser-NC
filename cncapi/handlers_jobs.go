package cncapi

// Shared logic behind /api/cnc/jobs and /api/cnc/jobs/stats. Mirrors
// http/cnc_jobs.go. Neither of these touches the resolver, authz, or
// settings store — they read the per-machine JSONL job log directly
// off disk by machine ID — so the only thing worth sharing is the
// limit/days parsing + defaulting, which is trivial but was
// duplicated nowhere before; this is the first second caller.

import "github.com/filebrowser/filebrowser/v2/cnc"

const (
	DefaultJobsLimit = 50
	MaxJobsLimit     = 500
	DefaultStatsDays = 30
)

// JobsListBody is the wire shape for GET /api/cnc/jobs.
type JobsListBody struct {
	MachineID string                `json:"machine_id"`
	Limit     int                   `json:"limit"`
	Count     int                   `json:"count"`
	Entries   []cnc.JobHistoryEntry `json:"entries"`
}

// ClampJobsLimit applies the same bounds as cncJobsListHandler:
// limit <= 0 -> DefaultJobsLimit, > MaxJobsLimit -> MaxJobsLimit.
func ClampJobsLimit(limit int) int {
	if limit <= 0 {
		return DefaultJobsLimit
	}
	if limit > MaxJobsLimit {
		return MaxJobsLimit
	}
	return limit
}

// JobsList reads machineID's job history. Mirrors cncJobsListHandler.
func (d Deps) JobsList(machineID string, limit int) (JobsListBody, int, error) {
	_, resolvedID, status, err := d.ResolveStreamer(machineID)
	if err != nil {
		return JobsListBody{}, status, err
	}
	limit = ClampJobsLimit(limit)
	entries, err := cnc.ReadJobHistory(resolvedID, limit)
	if err != nil {
		return JobsListBody{}, 500, err
	}
	return JobsListBody{MachineID: resolvedID, Limit: limit, Count: len(entries), Entries: entries}, 0, nil
}

// JobsStats computes machineID's windowed job stats. days < 0 is
// treated as DefaultStatsDays (matches cncJobsStatsHandler's "don't
// error a careless URL" behavior); 0 means all-time.
func (d Deps) JobsStats(machineID string, days int) (*cnc.JobStats, int, error) {
	_, resolvedID, status, err := d.ResolveStreamer(machineID)
	if err != nil {
		return nil, status, err
	}
	if days < 0 {
		days = DefaultStatsDays
	}
	stats, err := cnc.ComputeJobStats(resolvedID, days)
	if err != nil {
		return nil, 500, err
	}
	return stats, 0, nil
}
