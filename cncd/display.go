package cncd

import (
	"net/http"

	"github.com/gorilla/mux"

	"github.com/filebrowser/filebrowser/v2/settings"
)

// displayFetchHandler serves GET /api/displays/{id} — the one-call
// firmware endpoint bundling display config + tool-list data. Mirrors
// http/cnc_displays.go's cncDisplayFetchHandler exactly: same token
// gate (empty Display.Token means LAN-permissive, matching Token set
// requires it in the header or ?token=), same TouchDisplay liveness
// recording, same response shape ({"config":...,"data":...}), same
// Cache-Control: no-store.
//
// Unlike fbhttp's version this needs no firstAdminUser hack — the
// tool-table directory is resolved through a root-jailed
// cncapi.PathResolver (see toollist.go's buildMachineToolList), which
// never needed a user in the first place.
func (d Deps) displayFetchHandler(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	if id == "" {
		writeError(w, http.StatusBadRequest, nil)
		return
	}
	cfg := d.Config.Snapshot()
	idx := findDisplayIndex(cfg, id)
	if idx < 0 {
		writeError(w, http.StatusNotFound, nil)
		return
	}
	disp := cfg.Displays[idx]
	if disp.Token != "" {
		presented := extractBearer(r)
		if presented == "" {
			presented = r.URL.Query().Get("token")
		}
		if presented != disp.Token {
			writeError(w, http.StatusUnauthorized, nil)
			return
		}
	}
	d.Registry.TouchDisplay(id)

	payload, err := buildMachineToolList(d.Registry, cfg, d.Root, disp.MachineID)
	if err != nil {
		status := http.StatusInternalServerError
		if err == errMachineNotFound {
			status = http.StatusNotFound
		}
		writeError(w, status, err)
		return
	}

	resolved := disp.Resolved()
	resolved.Token = ""
	body := map[string]any{
		"config": resolved,
		"data":   payload,
	}
	w.Header().Set("Cache-Control", "no-store")
	_ = renderJSON(w, body)
}

// findDisplayIndex mirrors http/cnc_displays.go's function of the
// same name.
func findDisplayIndex(c settings.Cnc, id string) int {
	for i := range c.Displays {
		if c.Displays[i].ID == id {
			return i
		}
	}
	return -1
}
