package cncd

import (
	"net/http"

	"github.com/gorilla/mux"

	"github.com/filebrowser/filebrowser/v2/cncapi"
)

// displayFetchHandler serves GET /api/displays/{id} — the one-call
// firmware endpoint bundling display config + tool-list data. Mirrors
// http/cnc_displays.go's cncDisplayFetchHandler exactly: same token
// gate (empty Display.Token means LAN-permissive, matching Token set
// requires it in the header or ?token=), same TouchDisplay liveness
// recording, same response shape ({"config":...,"data":...}), same
// Cache-Control: no-store. The tool-list build itself
// (cncapi.Deps.BuildMachineToolList) is shared verbatim with
// filebrowser's version — this handler only owns the token gate and
// response envelope, which differ per host (no *users.User here).
func (d Deps) displayFetchHandler(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	if id == "" {
		writeError(w, http.StatusBadRequest, nil)
		return
	}
	cfg := d.Config.Snapshot()
	idx := cncapi.FindDisplayIndex(cfg, id)
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

	cd := cncapi.Deps{
		Registry: d.Registry,
		Resolver: cncapi.NewRootResolver(d.Root),
		Store:    d.Config,
	}
	payload, err := cd.BuildMachineToolList(disp.MachineID)
	if err != nil {
		status := http.StatusInternalServerError
		if err == cncapi.ErrMachineNotFound {
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
