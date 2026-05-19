package fbhttp

// Job history + stats endpoints.
//
//	GET /api/cnc/jobs?machine_id=...&limit=100   — recent rows, newest first
//	GET /api/cnc/jobs/stats?machine_id=...&days=7 — windowed aggregates
//
// Both read from the per-machine JSONL log the streamer writes at job
// end. No controller traffic; safe to call during a streaming job.

import (
	"net/http"
	"strconv"

	"github.com/filebrowser/filebrowser/v2/cnc"
)

const (
	defaultJobsLimit = 50
	maxJobsLimit     = 500
	defaultStatsDays = 30
)

func cncJobsListHandler(registry *cnc.Registry) handleFunc {
	return withUser(func(w http.ResponseWriter, r *http.Request, _ *data) (int, error) {
		_, machineID, code, err := resolveStreamer(registry, r)
		if err != nil {
			return code, err
		}
		q := r.URL.Query()
		limit := defaultJobsLimit
		if v := q.Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				if n > maxJobsLimit {
					n = maxJobsLimit
				}
				limit = n
			}
		}
		entries, err := cnc.ReadJobHistory(machineID, limit)
		if err != nil {
			return http.StatusInternalServerError, err
		}
		return renderJSON(w, r, map[string]any{
			"machine_id": machineID,
			"limit":      limit,
			"count":      len(entries),
			"entries":    entries,
		})
	})
}

func cncJobsStatsHandler(registry *cnc.Registry) handleFunc {
	return withUser(func(w http.ResponseWriter, r *http.Request, _ *data) (int, error) {
		_, machineID, code, err := resolveStreamer(registry, r)
		if err != nil {
			return code, err
		}
		days := defaultStatsDays
		if v := r.URL.Query().Get("days"); v != "" {
			// 0 days = all-time; negative is treated as default rather
			// than rejected so a careless URL doesn't error the page.
			if n, err := strconv.Atoi(v); err == nil && n >= 0 {
				days = n
			}
		}
		stats, err := cnc.ComputeJobStats(machineID, days)
		if err != nil {
			return http.StatusInternalServerError, err
		}
		return renderJSON(w, r, stats)
	})
}
