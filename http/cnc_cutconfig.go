package fbhttp

// /api/cnc/cutconfig — manage the active Toolpath cut config (.cutconfig ZIP)
// that drives geometry-based tool reconciliation. GET reports what's loaded;
// PUT (admin) uploads a new one; DELETE (admin) clears it.

import (
	"errors"
	"io"
	"net/http"

	"github.com/filebrowser/filebrowser/v2/cnc"
)

// cutConfigUploadCap bounds an upload. A real .cutconfig is a few hundred KB to
// ~25 MB (a big recommendation library inflates it), so allow generous room.
const cutConfigUploadCap = 64 << 20 // 64 MiB

func cncCutConfigGetHandler(registry *cnc.Registry) handleFunc {
	return withUser(func(w http.ResponseWriter, r *http.Request, _ *data) (int, error) {
		store := registry.CutConfigStore()
		if store == nil {
			return renderJSON(w, r, map[string]any{"loaded": false})
		}
		cfg, stale := store.Current()
		if cfg == nil {
			return renderJSON(w, r, map[string]any{"loaded": false, "error": store.LastError()})
		}
		return renderJSON(w, r, map[string]any{
			"loaded":     true,
			"name":       cfg.Name,
			"stale":      stale,
			"tool_count": len(cfg.Tools),
		})
	})
}

func cncCutConfigPutHandler(registry *cnc.Registry) handleFunc {
	return withAdmin(func(w http.ResponseWriter, r *http.Request, _ *data) (int, error) {
		store := registry.CutConfigStore()
		if store == nil {
			return http.StatusServiceUnavailable, errors.New("cut-config store not initialised")
		}
		r.Body = http.MaxBytesReader(w, r.Body, cutConfigUploadCap)
		buf, err := io.ReadAll(r.Body)
		if err != nil {
			return http.StatusBadRequest, err
		}
		cfg, err := store.Replace(buf)
		if err != nil {
			return http.StatusBadRequest, err
		}
		return renderJSON(w, r, map[string]any{
			"loaded":     true,
			"name":       cfg.Name,
			"tool_count": len(cfg.Tools),
		})
	})
}

func cncCutConfigDeleteHandler(registry *cnc.Registry) handleFunc {
	return withAdmin(func(w http.ResponseWriter, r *http.Request, _ *data) (int, error) {
		store := registry.CutConfigStore()
		if store == nil {
			return http.StatusServiceUnavailable, errors.New("cut-config store not initialised")
		}
		if err := store.Clear(); err != nil {
			return http.StatusInternalServerError, err
		}
		return renderJSON(w, r, map[string]bool{"cleared": true})
	})
}
