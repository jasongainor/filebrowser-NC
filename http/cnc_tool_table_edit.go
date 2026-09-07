package fbhttp

// Local-only tool-table edits.
//
// Operators sometimes need to tweak an offset without round-tripping
// through the controller: copying a sister-tool's offsets into a fresh
// pocket, recording a hand-measurement when the spindle probe is out,
// or restoring a known-good value after a chip strike. None of those
// touch the machine — this endpoint only writes the edit into a new
// history dump so the dashboard reflects the operator's intent.
//
// Write-back to the controller (G10 emission) is a deliberate future
// phase: see project_filebrowser_nc_tooltable_edit_todo.md. Until that
// lands, an edit is purely a dashboard override that the next
// controller read will replace.
//
// Logic lives in cncapi.Deps.ToolTableEdit, shared with cncd; this is
// a thin wrapper.

import (
	"encoding/json"
	"net/http"

	"github.com/filebrowser/filebrowser/v2/cnc"
	"github.com/filebrowser/filebrowser/v2/cncapi"
)

func cncToolTableEditHandler(registry *cnc.Registry) handleFunc {
	return withUser(func(w http.ResponseWriter, r *http.Request, d *data) (int, error) {
		if !d.authz().CanModify() {
			return http.StatusForbidden, nil
		}
		var req cncapi.ToolTableEditRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			return http.StatusBadRequest, err
		}
		tbl, code, err := d.cncapiDeps(registry).ToolTableEdit(req, r.URL.Query().Get("machine_id"))
		if err != nil {
			return code, err
		}
		return renderJSON(w, r, map[string]any{"table": tbl})
	})
}
