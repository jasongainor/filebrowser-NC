package fbhttp

// GET /api/machines/{id}/toollist
//
// Machine-scoped reconciled tool-list view. Reads the latest persisted
// tool-table dump for the machine + the operator's Fusion library
// (shared across machines) and produces a display-agnostic JSON payload
// per cnc.ToolList. Drives:
//   - The dashboard's tool-list panel
//   - The reTerminal e-paper firmware (via /api/displays/{id})
//   - Any future kiosk / browser view that wants the same contract
//
// The engine (buildMachineToolList) now lives in
// cncapi.Deps.BuildMachineToolList, shared verbatim with cncd
// (cncd/display.go, cncd/router_cnc.go). This file keeps the
// filebrowser-specific glue: the mux route and the *data seam.

import (
	"errors"
	"net/http"

	"github.com/gorilla/mux"

	"github.com/filebrowser/filebrowser/v2/cnc"
	"github.com/filebrowser/filebrowser/v2/cncapi"
)

func cncMachineToolListHandler(registry *cnc.Registry) handleFunc {
	return withUser(func(w http.ResponseWriter, r *http.Request, d *data) (int, error) {
		machineID := mux.Vars(r)["id"]
		if machineID == "" {
			return http.StatusBadRequest, errors.New("machine id required")
		}
		payload, err := d.cncapiDeps(registry).BuildMachineToolList(machineID)
		if err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, cncapi.ErrMachineNotFound) {
				status = http.StatusNotFound
			}
			return status, err
		}
		return renderJSON(w, r, payload)
	})
}
