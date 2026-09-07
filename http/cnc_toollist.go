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

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"

	"github.com/gorilla/mux"

	"github.com/filebrowser/filebrowser/v2/cnc"
	"github.com/filebrowser/filebrowser/v2/settings"
)

func cncMachineToolListHandler(registry *cnc.Registry) handleFunc {
	return withUser(func(w http.ResponseWriter, r *http.Request, d *data) (int, error) {
		machineID := mux.Vars(r)["id"]
		if machineID == "" {
			return http.StatusBadRequest, errors.New("machine id required")
		}
		payload, status, err := buildMachineToolList(registry, d, machineID)
		if err != nil {
			return status, err
		}
		return renderJSON(w, r, payload)
	})
}

// buildMachineToolList is the engine that both the per-machine
// /toollist handler AND the /displays/{id} handler share. Returns
// (payload, status, err). On 404 (unknown machine) the payload is nil.
func buildMachineToolList(registry *cnc.Registry, d *data, machineID string) (*cnc.ToolList, int, error) {
	m := findMachine(d.settings, machineID)
	if m == nil {
		return nil, http.StatusNotFound, errors.New("machine not found")
	}

	// Connected = the controller actually answered recently. The Link
	// tracks the last successful Q-code round-trip and the always-on
	// baseline poller keeps that timestamp fresh, so this is true
	// whenever the machine is genuinely reachable — no operator and no
	// open dashboard required.
	//
	// This used to read ag.IsAwake(), i.e. "has a human touched the
	// dashboard in the last 5 minutes". Non-interactive consumers could
	// never see connected=true: the e-paper display polls once every
	// ~100 minutes and would essentially never land inside a wake
	// window, so it rendered "[disconnected]" against a running mill.
	connected := false
	if st, _ := registry.Streamer(m.ID); st != nil {
		connected = st.Alive()
	}

	// Latest table — may be nil for a fresh install. ReadJobHistory's
	// "missing file is not an error" semantics apply here too.
	var tbl *cnc.ToolTable
	dir, err := toolTableDirAbs(d, m.ID)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	if latestPath, _ := newestJSONIn(dir); latestPath != "" {
		if buf, err := os.ReadFile(latestPath); err == nil {
			var t cnc.ToolTable
			if err := json.Unmarshal(buf, &t); err == nil {
				tbl = &t
			}
		}
	}

	// Library is shared across machines today (one operator, one
	// Fusion library file). LibraryStore returns nil on a fresh install.
	var lib *cnc.ToolLibrary
	if store := registry.LibraryStore(); store != nil {
		lib = store.Library()
	}

	units := effectiveUnits(m)
	pocketCount := m.EffectiveToolSlots()
	out := cnc.BuildToolList(
		m.ID,
		m.Name,
		units,
		connected,
		pocketCount,
		200, // Haas NGC table max
		tbl,
		lib,
	)
	return out, http.StatusOK, nil
}

// effectiveUnits returns "in" or "mm". No per-machine "units" field
// exists yet — Haas defaults to inches in NGC and a metric machine
// flips this via Setting 9. For now we honor an explicit env override
// (CNC_UNITS=mm) but otherwise return inches.
func effectiveUnits(_ *settings.Machine) string {
	if u := os.Getenv("CNC_UNITS"); u == "mm" || u == "in" {
		return u
	}
	return "in"
}
