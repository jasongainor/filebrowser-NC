package fbhttp

// /api/cnc/auto-send — opt-in pipeline that bundles preflight + start
// into a single round-trip. When the machine has AutoSendEnabled and
// preflight comes back all-green (no missing / empty / warn tools and
// no pending spindle swap), the file goes straight to MEM-tab Receive.
//
// CYCLE START is NOT triggered remotely — Haas doesn't expose that over
// RS-232 in a safe way. Operators still press the physical button. The
// "auto" here means "skip the wizard click-through", not "auto-cycle".
//
// Refusals are explicit and pre-emptive: the response body always
// includes the preflight summary + reason so the UI can fall back to
// the normal wizard flow without a second round-trip.
//
// Logic lives in cncapi.Deps.AutoSend / cncapi.AutoSendBlockReason,
// shared with cncd; this file is a thin wrapper plus the response
// envelope's HTTP status handling (202 on start).

import (
	"encoding/json"
	"net/http"

	"github.com/filebrowser/filebrowser/v2/cnc"
	"github.com/filebrowser/filebrowser/v2/cncapi"
)

// cncAutoSendHandler runs preflight, evaluates the auto-send gate,
// and either starts the job or returns the block reason. Returns 202
// Accepted when the job starts; 409 Conflict when gated; 400/404 on
// bad input. Always renders a cncapi.AutoSendResponse so the client
// can surface the preflight summary either way.
func cncAutoSendHandler(registry *cnc.Registry) handleFunc {
	return withUser(func(w http.ResponseWriter, r *http.Request, d *data) (int, error) {
		if !d.authz().CanModify() {
			return http.StatusForbidden, nil
		}
		req := &cncapi.AutoSendRequest{}
		if err := json.NewDecoder(r.Body).Decode(req); err != nil {
			return http.StatusBadRequest, err
		}
		body, status, err := d.cncapiDeps(registry).AutoSend(*req, r.URL.Query().Get("machine_id"))
		if err != nil {
			return status, err
		}
		if status == http.StatusAccepted {
			w.WriteHeader(http.StatusAccepted)
		}
		return renderJSON(w, r, body)
	})
}

// autoSendBlockReason is kept under its original name (rather than
// inlined as cncapi.AutoSendBlockReason at every call site) so
// cnc_autosend_test.go keeps testing this exact code path.
func autoSendBlockReason(pf *cnc.Preflight) string {
	return cncapi.AutoSendBlockReason(pf)
}
