package fbhttp

// /api/cnc/tool-library — operator uploads their Fusion 360 tool
// library export so the dashboard can enrich live tool-table rows
// with descriptions, vendor links, flute counts, and (eventually) a
// revolved-profile SVG. Admin-only on PUT/DELETE; user-readable on GET.
//
// GET/PUT logic lives in cncapi.Deps.ToolLibraryGet/Put, shared with
// cncd; DELETE and the per-slot lookup have no cncd route yet (see
// docs/CNCD.md) so they stay filebrowser-only, using the registry
// directly.

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/filebrowser/filebrowser/v2/cnc"
	"github.com/filebrowser/filebrowser/v2/cncapi"
)

func cncToolLibraryGetHandler(registry *cnc.Registry) handleFunc {
	return withUser(func(w http.ResponseWriter, r *http.Request, d *data) (int, error) {
		return renderJSON(w, r, d.cncapiDeps(registry).ToolLibraryGet())
	})
}

// cncToolLibrarySlotHandler returns the entry for one pocket number.
// /api/cnc/tool-library/slot/{n}. 404 when no library or no slot.
func cncToolLibrarySlotHandler(registry *cnc.Registry) handleFunc {
	return withUser(func(w http.ResponseWriter, r *http.Request, _ *data) (int, error) {
		store := registry.LibraryStore()
		if store == nil {
			return http.StatusNotFound, nil
		}
		lib := store.Library()
		if lib == nil {
			return http.StatusNotFound, nil
		}
		// Path tail: /api/cnc/tool-library/slot/{n}
		path := r.URL.Path
		idx := strings.LastIndex(path, "/")
		if idx < 0 || idx+1 >= len(path) {
			return http.StatusBadRequest, errors.New("missing slot number")
		}
		n, err := strconv.Atoi(path[idx+1:])
		if err != nil {
			return http.StatusBadRequest, err
		}
		entry, ok := lib.Lookup(n)
		if !ok {
			return http.StatusNotFound, nil
		}
		return renderJSON(w, r, entry)
	})
}

// cncToolLibraryPutHandler replaces the stored library with the
// supplied JSON. Admin-only.
func cncToolLibraryPutHandler(registry *cnc.Registry) handleFunc {
	return withAdmin(func(w http.ResponseWriter, r *http.Request, d *data) (int, error) {
		r.Body = http.MaxBytesReader(w, r.Body, cncapi.ToolLibraryUploadCap)
		buf, err := io.ReadAll(r.Body)
		if err != nil {
			return http.StatusBadRequest, err
		}
		body, code, err := d.cncapiDeps(registry).ToolLibraryPut(buf)
		if err != nil {
			return code, err
		}
		return renderJSON(w, r, body)
	})
}

// cncToolLibraryDeleteHandler clears the stored library. Admin-only.
func cncToolLibraryDeleteHandler(registry *cnc.Registry) handleFunc {
	return withAdmin(func(w http.ResponseWriter, r *http.Request, _ *data) (int, error) {
		store := registry.LibraryStore()
		if store == nil {
			return http.StatusServiceUnavailable, errors.New("tool-library store not initialised")
		}
		if err := store.Clear(); err != nil {
			return http.StatusInternalServerError, err
		}
		return renderJSON(w, r, map[string]bool{"cleared": true})
	})
}
