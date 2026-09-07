package fbhttp

// Displays CRUD + one-call /api/displays/{id} that bundles config +
// data for the on-device firmware.
//
// Endpoints:
//   GET    /api/cnc/displays         — admin: list all
//   POST   /api/cnc/displays         — admin: create
//   PUT    /api/cnc/displays/{id}    — admin: update
//   DELETE /api/cnc/displays/{id}    — admin: remove
//   GET    /api/displays/{id}        — firmware: { config, data }
//
// The unauthenticated /api/displays/{id} read is what the e-paper hits.
// When the Display has a Token set the request must carry it; without
// a token the read is LAN-permissive (shop networks are isolated and
// adding TLS / auth to an ESP32 device would price out the use case).
//
// CRUD logic lives in cncapi.Deps.Displays*, shared with cncd
// (cncd/router_cnc.go); this file is a thin wrapper. The firmware
// fetch handler shares its tool-list build (cncapi.Deps.BuildMachineToolList)
// with cncd/display.go but keeps its own token-gate/response glue —
// that plumbing differs per host (no *users.User on cncd) rather than
// being duplicated logic.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/gorilla/mux"

	"github.com/filebrowser/filebrowser/v2/cnc"
	"github.com/filebrowser/filebrowser/v2/cncapi"
	"github.com/filebrowser/filebrowser/v2/settings"
)

// cncDisplaysListHandler returns the configured displays (admin only).
// Tokens are passed through verbatim — the admin UI surfaces them so
// the operator can flash them onto the SD card.
func cncDisplaysListHandler(registry *cnc.Registry) handleFunc {
	return withAdmin(func(w http.ResponseWriter, r *http.Request, d *data) (int, error) {
		out := d.cncapiDeps(registry).DisplaysList()
		return renderJSON(w, r, map[string]any{"displays": out})
	})
}

// cncDisplaysCreateHandler, like the original, has no *cnc.Registry in
// scope (it isn't threaded through this route's constructor) — passing
// nil into cncapiDeps is safe because DisplaysCreate/Update/Delete
// never touch Deps.Registry (only DisplaysList does, for LastSeen).
func cncDisplaysCreateHandler() handleFunc {
	return withAdmin(func(w http.ResponseWriter, r *http.Request, d *data) (int, error) {
		var req settings.Display
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			return http.StatusBadRequest, err
		}
		disp, code, err := d.cncapiDeps(nil).DisplaysCreate(req, newDisplayID())
		if err != nil {
			return code, err
		}
		return renderJSON(w, r, disp)
	})
}

func cncDisplaysUpdateHandler() handleFunc {
	return withAdmin(func(w http.ResponseWriter, r *http.Request, d *data) (int, error) {
		id := mux.Vars(r)["id"]
		var req settings.Display
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			return http.StatusBadRequest, err
		}
		disp, code, err := d.cncapiDeps(nil).DisplaysUpdate(id, req)
		if err != nil {
			return code, err
		}
		return renderJSON(w, r, disp)
	})
}

func cncDisplaysDeleteHandler() handleFunc {
	return withAdmin(func(_ http.ResponseWriter, r *http.Request, d *data) (int, error) {
		code, err := d.cncapiDeps(nil).DisplaysDelete(mux.Vars(r)["id"])
		if err != nil {
			return code, err
		}
		return 0, nil
	})
}

// cncDisplayFetchHandler is the unauthenticated firmware endpoint:
//
//	GET /api/displays/{id}
//
// Returns { config, data } in a single round trip so the e-paper
// firmware doesn't have to chain calls. config = resolved-defaults
// view of the Display struct; data = the embedded ToolList payload
// for the display's machine.
//
// Token check is hand-rolled (not wrapped in withUser) because the
// firmware doesn't have a filebrowser session — it carries its own
// Display.Token in Authorization: Bearer or ?token=. monkey() still
// hydrates d.settings so we get the displays list cheaply.
func cncDisplayFetchHandler(registry *cnc.Registry) handleFunc {
	return func(w http.ResponseWriter, r *http.Request, d *data) (int, error) {
		id := mux.Vars(r)["id"]
		if id == "" {
			return http.StatusBadRequest, errors.New("display id required")
		}
		idx := cncapi.FindDisplayIndex(d.settings.Cnc, id)
		if idx < 0 {
			return http.StatusNotFound, errors.New("display not found")
		}
		disp := d.settings.Cnc.Displays[idx]
		// Token gate. When the admin set a token on the Display, the
		// request must present it. Empty token = LAN-permissive — shop
		// networks are isolated and TLS on a battery-friendly ESP32
		// device prices out the use case.
		if disp.Token != "" {
			presented := extractBearer(r)
			if presented == "" {
				presented = r.URL.Query().Get("token")
			}
			if presented != disp.Token {
				return http.StatusUnauthorized, nil
			}
		}
		// Record the poll now that the display has proven itself (token
		// checked out, or none was required). In-memory only — see
		// Registry.TouchDisplay for why this doesn't touch settings.Save().
		registry.TouchDisplay(id)
		// This handler isn't wrapped in withUser (no JWT from the
		// e-paper), so d.user is nil here; d.pathResolver()'s fallback
		// for that case is a resolver rooted at the server root —
		// exactly the scope tool-table dumps are written under, and
		// exactly what an admin user's FullPath would have resolved to.
		payload, err := d.cncapiDeps(registry).BuildMachineToolList(disp.MachineID)
		if err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, cncapi.ErrMachineNotFound) {
				status = http.StatusNotFound
			}
			return status, err
		}
		// Resolve defaults into the wire-side config so the firmware
		// never has to know what's a default and what's explicit.
		cfg := disp.Resolved()
		// Don't leak the token back over the wire — the firmware
		// already has it locally; downstream operators inspecting the
		// HTTP capture shouldn't see it.
		cfg.Token = ""
		body := map[string]any{
			"config": cfg,
			"data":   payload,
		}
		// Long cache-control would interact badly with the firmware's
		// own "poll every N seconds" loop; let it author the cadence.
		w.Header().Set("Cache-Control", "no-store")
		return renderJSON(w, r, body)
	}
}

// newDisplayID mirrors cncapi.NewDisplayID under its original name.
func newDisplayID() string {
	return cncapi.NewDisplayID()
}

// extractBearer pulls the token out of `Authorization: Bearer <t>`,
// returning "" when the header is missing or malformed.
func extractBearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h == "" {
		return ""
	}
	parts := strings.SplitN(h, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return strings.TrimSpace(parts[1])
}
