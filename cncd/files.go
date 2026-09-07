package cncd

// GET/PUT/DELETE /api/files — a minimal, root-jailed file API standing
// in for the UI cncd doesn't have yet. Every path is resolved through
// a cncapi.PathResolver rooted at the single directory cncd serves,
// so "?path=../../etc/passwd" is rejected before any syscall touches
// it (see cncapi.RootResolver.FullPath).
//
// GET is open to any caller that can reach the daemon — read access to
// the served directory has no more of a threat model than the
// existing Display-token-less firmware endpoint (LAN-only shop
// network). PUT and DELETE mutate, so they're gated the same way
// every other mutating cncd route is: the caller's Authz must report
// CanModify(), which today only a matching machine-token bearer gets.

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/filebrowser/filebrowser/v2/cncapi"
)

// fileEntry is one row of a GET /api/files directory listing.
type fileEntry struct {
	Name    string    `json:"name"`
	IsDir   bool      `json:"isDir"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"modTime"`
}

func queryPath(r *http.Request) string {
	p := r.URL.Query().Get("path")
	if p == "" {
		return "/"
	}
	return p
}

// filesListHandler lists the directory at ?path= (default: root).
// Listing a file (rather than a directory) returns its own info as a
// single-entry list, matching the common "stat-or-list" convenience
// of file APIs like this one.
func (d Deps) filesListHandler(resolver cncapi.PathResolver) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rel := queryPath(r)
		abs, err := resolver.FullPath(rel)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		info, err := os.Stat(abs)
		if err != nil {
			writeError(w, statusForFileErr(err), err)
			return
		}
		if !info.IsDir() {
			_ = renderJSON(w, map[string]any{
				"path": rel,
				"entries": []fileEntry{{
					Name: info.Name(), IsDir: false, Size: info.Size(), ModTime: info.ModTime(),
				}},
			})
			return
		}
		dirEntries, err := os.ReadDir(abs)
		if err != nil {
			writeError(w, statusForFileErr(err), err)
			return
		}
		out := make([]fileEntry, 0, len(dirEntries))
		for _, de := range dirEntries {
			fi, err := de.Info()
			if err != nil {
				continue
			}
			out = append(out, fileEntry{
				Name: de.Name(), IsDir: de.IsDir(), Size: fi.Size(), ModTime: fi.ModTime(),
			})
		}
		_ = renderJSON(w, map[string]any{"path": rel, "entries": out})
	}
}

// filesUploadHandler writes the request body to ?path=, creating
// parent directories as needed. Requires CanModify().
func (d Deps) filesUploadHandler(resolver cncapi.PathResolver) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !authzFor(r, d.Config.Snapshot().MachineToken).CanModify() {
			writeError(w, http.StatusForbidden, nil)
			return
		}
		rel := r.URL.Query().Get("path")
		if rel == "" || rel == "/" {
			writeError(w, http.StatusBadRequest, errors.New("path required"))
			return
		}
		abs, err := resolver.FullPath(rel)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		f, err := os.OpenFile(abs, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
		if err != nil {
			writeError(w, statusForFileErr(err), err)
			return
		}
		defer f.Close()
		n, err := io.Copy(f, r.Body)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		_ = renderJSON(w, map[string]any{"path": rel, "size": n})
	}
}

// filesDeleteHandler removes the single file at ?path=. Refuses to
// recurse into directories — a directory delete is a bigger blast
// radius than this minimal API should hand out silently; that can
// come later once there's a UI confirming it. Requires CanModify().
func (d Deps) filesDeleteHandler(resolver cncapi.PathResolver) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !authzFor(r, d.Config.Snapshot().MachineToken).CanModify() {
			writeError(w, http.StatusForbidden, nil)
			return
		}
		rel := r.URL.Query().Get("path")
		if rel == "" || rel == "/" {
			writeError(w, http.StatusBadRequest, errors.New("path required"))
			return
		}
		abs, err := resolver.FullPath(rel)
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
			writeError(w, http.StatusBadRequest, errors.New("refusing to delete a directory"))
			return
		}
		if err := os.Remove(abs); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		_ = renderJSON(w, map[string]bool{"deleted": true})
	}
}

func statusForFileErr(err error) int {
	switch {
	case os.IsNotExist(err):
		return http.StatusNotFound
	case os.IsPermission(err):
		return http.StatusForbidden
	default:
		return http.StatusInternalServerError
	}
}
