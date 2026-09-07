package cncd

// registerJobs mounts the job-bucketed-root API: GET /api/jobs (the
// per-folder summary the future UI and the operator's "what's queued"
// glance both want), POST /api/jobs (create a job folder ahead of
// time — idempotent), and POST /api/jobs/file (file one loose root
// file into its folder by hand, for the case an operator doesn't want
// to wait out the auto-bucket watcher's poll interval). See
// cncd/jobs.go for the actual folder/move logic and docs/JOB_FOLDERS.md
// for the operator-facing story.
//
// Gating follows the same two-tier posture as GET/PUT/DELETE
// /api/files (cncd/files.go): listing is open (LAN-permissive, no
// more of a threat model than browsing the share itself), the two
// writes require CanModify() — a matching machine-token bearer or a
// signed-in session (cncd/auth.go).
import (
	"errors"
	"net/http"
	"os"
	"path/filepath"

	"github.com/gorilla/mux"

	"github.com/filebrowser/filebrowser/v2/cncapi"
)

func registerJobs(r *mux.Router, d Deps) {
	api := r.PathPrefix("/api").Subrouter()
	api.HandleFunc("/jobs", d.jobFoldersListHandler()).Methods("GET")
	api.HandleFunc("/jobs", d.jobFoldersCreateHandler()).Methods("POST")
	api.HandleFunc("/jobs/file", d.jobFoldersFileHandler()).Methods("POST")
}

// jobFoldersListHandler serves GET /api/jobs — see buildJobsList.
func (d Deps) jobFoldersListHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		result, err := buildJobsList(d)
		if err != nil {
			writeError(w, statusForFileErr(err), err)
			return
		}
		_ = renderJSON(w, result)
	}
}

type jobsCreateBody struct {
	Job  string `json:"job"`
	Part string `json:"part,omitempty"`
}

// jobFoldersCreateHandler serves POST /api/jobs {job, part} — creates the
// job folder ahead of a post ever landing a file in it. Idempotent:
// an already-existing folder is a 200, not a 409.
func (d Deps) jobFoldersCreateHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !authzFor(r, d.Config.Snapshot().MachineToken).CanModify() {
			writeError(w, http.StatusForbidden, nil)
			return
		}
		var body jobsCreateBody
		if !decodeJSON(w, r, &body) {
			return
		}
		if body.Job == "" {
			writeError(w, http.StatusBadRequest, errors.New("job is required"))
			return
		}
		name, created, err := createJobFolder(d.Root, body.Job, body.Part)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		_ = renderJSON(w, map[string]any{"name": name, "created": created})
	}
}

type jobsFileBody struct {
	Path string `json:"path"`
}

// jobFoldersFileHandler serves POST /api/jobs/file {path} — files the one
// loose root file at path into its job folder, based on its GMW
// header. path must resolve to a file whose parent is the served
// root itself; anything already inside a subfolder is rejected rather
// than silently re-filed (this daemon never moves a file that's
// already somewhere on purpose).
func (d Deps) jobFoldersFileHandler() http.HandlerFunc {
	resolver := cncapi.NewRootResolver(d.Root)
	return func(w http.ResponseWriter, r *http.Request) {
		if !authzFor(r, d.Config.Snapshot().MachineToken).CanModify() {
			writeError(w, http.StatusForbidden, nil)
			return
		}
		var body jobsFileBody
		if !decodeJSON(w, r, &body) {
			return
		}
		if body.Path == "" {
			writeError(w, http.StatusBadRequest, errors.New("path is required"))
			return
		}
		abs, err := resolver.FullPath(body.Path)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		info, err := os.Stat(abs)
		if err != nil {
			writeError(w, statusForFileErr(err), err)
			return
		}
		if info.IsDir() {
			writeError(w, http.StatusBadRequest, errors.New("refusing to file a directory"))
			return
		}
		cleanRoot := filepath.Clean(d.Root)
		if filepath.Dir(abs) != cleanRoot {
			writeError(w, http.StatusBadRequest, errors.New("path is not a loose file at the served root"))
			return
		}
		destRel, err := fileLooseRootFile(d.Root, abs)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		_ = renderJSON(w, map[string]any{"path": destRel})
	}
}
