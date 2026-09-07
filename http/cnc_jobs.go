package fbhttp

// Job history + stats endpoints.
//
//	GET /api/cnc/jobs?machine_id=...&limit=100   — recent rows, newest first
//	GET /api/cnc/jobs/stats?machine_id=...&days=7 — windowed aggregates
//
// Both read from the per-machine JSONL log the streamer writes at job
// end. No controller traffic; safe to call during a streaming job.
//
// Logic lives in cncapi.Deps.JobsList/JobsStats, shared with cncd.

import (
	"net/http"
	"strconv"

	"github.com/filebrowser/filebrowser/v2/cnc"
)

func cncJobsListHandler(registry *cnc.Registry) handleFunc {
	return withUser(func(w http.ResponseWriter, r *http.Request, d *data) (int, error) {
		limit := 0
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				limit = n
			}
		}
		body, code, err := d.cncapiDeps(registry).JobsList(r.URL.Query().Get("machine_id"), limit)
		if err != nil {
			return code, err
		}
		return renderJSON(w, r, body)
	})
}

func cncJobsStatsHandler(registry *cnc.Registry) handleFunc {
	return withUser(func(w http.ResponseWriter, r *http.Request, d *data) (int, error) {
		days := -1
		if v := r.URL.Query().Get("days"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n >= 0 {
				days = n
			}
		}
		stats, code, err := d.cncapiDeps(registry).JobsStats(r.URL.Query().Get("machine_id"), days)
		if err != nil {
			return code, err
		}
		return renderJSON(w, r, stats)
	})
}
