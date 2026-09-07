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

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/mux"

	"github.com/filebrowser/filebrowser/v2/cnc"
	"github.com/filebrowser/filebrowser/v2/settings"
)

// displayListItem is a Display plus the process-local liveness the
// admin UI needs to render "last seen" / stale. See
// cnc.Registry.TouchDisplay for why LastSeen isn't part of the
// persisted settings.Display.
type displayListItem struct {
	settings.Display
	LastSeen *time.Time `json:"lastSeen,omitempty"`
}

// cncDisplaysListHandler returns the configured displays (admin only).
// Tokens are passed through verbatim — the admin UI surfaces them so
// the operator can flash them onto the SD card.
func cncDisplaysListHandler(registry *cnc.Registry) handleFunc {
	return withAdmin(func(w http.ResponseWriter, r *http.Request, d *data) (int, error) {
		disps := d.settings.Cnc.Displays
		out := make([]displayListItem, 0, len(disps))
		for _, disp := range disps {
			item := displayListItem{Display: disp}
			if seen, ok := registry.DisplayLastSeen(disp.ID); ok {
				t := seen
				item.LastSeen = &t
			}
			out = append(out, item)
		}
		return renderJSON(w, r, map[string]any{"displays": out})
	})
}

type displayUpsertBody struct {
	// On create, ID is generated; on update, the URL param wins.
	settings.Display
}

func cncDisplaysCreateHandler() handleFunc {
	return withAdmin(func(w http.ResponseWriter, r *http.Request, d *data) (int, error) {
		var req displayUpsertBody
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			return http.StatusBadRequest, err
		}
		if req.MachineID == "" {
			return http.StatusBadRequest, errors.New("machineId required")
		}
		if findMachine(d.settings, req.MachineID) == nil {
			return http.StatusBadRequest, errors.New("unknown machineId")
		}
		req.ID = newDisplayID()
		d.settings.Cnc.Displays = append(d.settings.Cnc.Displays, req.Display)
		if err := d.store.Settings.Save(d.settings); err != nil {
			return errToStatus(err), err
		}
		return renderJSON(w, r, req.Display)
	})
}

func cncDisplaysUpdateHandler() handleFunc {
	return withAdmin(func(w http.ResponseWriter, r *http.Request, d *data) (int, error) {
		id := mux.Vars(r)["id"]
		if id == "" {
			return http.StatusBadRequest, errors.New("display id required")
		}
		var req displayUpsertBody
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			return http.StatusBadRequest, err
		}
		if req.MachineID == "" {
			return http.StatusBadRequest, errors.New("machineId required")
		}
		if findMachine(d.settings, req.MachineID) == nil {
			return http.StatusBadRequest, errors.New("unknown machineId")
		}
		req.ID = id
		idx := findDisplayIndex(d.settings, id)
		if idx < 0 {
			return http.StatusNotFound, errors.New("display not found")
		}
		d.settings.Cnc.Displays[idx] = req.Display
		if err := d.store.Settings.Save(d.settings); err != nil {
			return errToStatus(err), err
		}
		return renderJSON(w, r, req.Display)
	})
}

func cncDisplaysDeleteHandler() handleFunc {
	return withAdmin(func(_ http.ResponseWriter, r *http.Request, d *data) (int, error) {
		id := mux.Vars(r)["id"]
		if id == "" {
			return http.StatusBadRequest, errors.New("display id required")
		}
		idx := findDisplayIndex(d.settings, id)
		if idx < 0 {
			return http.StatusNotFound, errors.New("display not found")
		}
		d.settings.Cnc.Displays = append(
			d.settings.Cnc.Displays[:idx],
			d.settings.Cnc.Displays[idx+1:]...,
		)
		if err := d.store.Settings.Save(d.settings); err != nil {
			return errToStatus(err), err
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
		idx := findDisplayIndex(d.settings, id)
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
		// Recorded even if the tool-list build below fails downstream:
		// the display reaching this endpoint at all is the liveness
		// signal, independent of whether we could answer it.
		registry.TouchDisplay(id)
		// buildMachineToolList resolves the tool-table dump directory
		// through d.pathResolver(). This handler isn't wrapped in
		// withUser (no JWT from the e-paper), so d.user is nil here;
		// pathResolver's fallback for that case is a resolver rooted at
		// the server root — exactly the scope tool-table dumps are
		// written under (see toolTableDirAbs), and exactly what an
		// admin user's FullPath would have resolved to. No user object
		// needs to be borrowed to reach it.
		payload, status, err := buildMachineToolList(registry, d, disp.MachineID)
		if err != nil {
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

// findDisplayIndex returns the index of the display whose ID matches,
// or -1 when none exists. Linear scan — displays are O(devices in shop).
func findDisplayIndex(s *settings.Settings, id string) int {
	for i := range s.Cnc.Displays {
		if s.Cnc.Displays[i].ID == id {
			return i
		}
	}
	return -1
}

// newDisplayID returns a 16-hex-char random ID. crypto/rand-backed so
// two admins creating displays simultaneously don't collide.
func newDisplayID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
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
