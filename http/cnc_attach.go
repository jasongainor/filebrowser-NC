package fbhttp

// /api/cnc/attach — operator-marked "this filebrowser file is what the
// controller is actually running, even though we didn't send it via
// the bridge." Drives the /machine dashboard's follow-along when an
// NC program was loaded from SD card / Ethernet drop.
//
// Two endpoints:
//   POST /api/cnc/attach   { file_path, source? }
//   DELETE /api/cnc/attach
//
// Attachment is cleared automatically when a real streaming job starts
// (the job's own file is authoritative). Manual detach is for operator
// recovery — e.g. they switched programs on the controller.
//
// Logic lives in cncapi.Deps.Attach/Detach, shared with cncd; these
// are thin wrappers.

import (
	"encoding/json"
	"net/http"

	"github.com/filebrowser/filebrowser/v2/cnc"
	"github.com/filebrowser/filebrowser/v2/cncapi"
)

// cncAttachHandler validates the file path against the user scope,
// marks it on the streamer, and emits a status broadcast so any open
// dashboards pick it up.
func cncAttachHandler(registry *cnc.Registry) handleFunc {
	return withUser(func(w http.ResponseWriter, r *http.Request, d *data) (int, error) {
		if !d.authz().CanModify() {
			return http.StatusForbidden, nil
		}
		req := &cncapi.AttachRequest{}
		if err := json.NewDecoder(r.Body).Decode(req); err != nil {
			return http.StatusBadRequest, err
		}
		st, code, err := d.cncapiDeps(registry).Attach(*req, r.URL.Query().Get("machine_id"))
		if err != nil {
			return code, err
		}
		return renderJSON(w, r, st)
	})
}

func cncDetachHandler(registry *cnc.Registry) handleFunc {
	return withUser(func(w http.ResponseWriter, r *http.Request, d *data) (int, error) {
		if !d.authz().CanModify() {
			return http.StatusForbidden, nil
		}
		st, code, err := d.cncapiDeps(registry).Detach(r.URL.Query().Get("machine_id"))
		if err != nil {
			return code, err
		}
		return renderJSON(w, r, st)
	})
}
