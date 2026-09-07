package fbhttp

// /api/cnc/queue/* — per-machine staging queue for NC sends.
//
// Shared across operators (one queue per machine, not per-user) so a
// second operator viewing /machine sees what the first staged.
// Persistence lives in cnc.QueueStore — see cnc/queue.go.
//
// Mutations broadcast a "queue" event on the per-machine WS stream so
// every connected client refreshes without polling.
//
// The actual logic is shared with cncd (cncd/router_cnc.go) via
// cncapi.Deps' Queue* methods — every handler here is a thin wrapper
// translating this package's request/response conventions
// (withUser/withAdmin, renderJSON, mux path vars) onto that shared
// core so behavior is unchanged.

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/filebrowser/filebrowser/v2/cnc"
	"github.com/filebrowser/filebrowser/v2/cncapi"
)

// cncQueueListHandler — GET /api/cnc/queue?machine_id=
func cncQueueListHandler(registry *cnc.Registry) handleFunc {
	return withUser(func(w http.ResponseWriter, r *http.Request, d *data) (int, error) {
		cd := d.cncapiDeps(registry)
		_, machineID, code, err := cd.ResolveStreamer(r.URL.Query().Get("machine_id"))
		if err != nil {
			return code, err
		}
		return renderJSON(w, r, cd.QueueList(machineID))
	})
}

// cncQueueAddHandler — POST /api/cnc/queue. Adds a file to the queue.
func cncQueueAddHandler(registry *cnc.Registry) handleFunc {
	return withUser(func(w http.ResponseWriter, r *http.Request, d *data) (int, error) {
		if !d.authz().CanModify() {
			return http.StatusForbidden, nil
		}
		req := &cncapi.QueueAddRequest{}
		if err := json.NewDecoder(r.Body).Decode(req); err != nil {
			return http.StatusBadRequest, err
		}
		item, code, err := d.cncapiDeps(registry).QueueAdd(*req, r.URL.Query().Get("machine_id"))
		if err != nil {
			return code, err
		}
		return renderJSON(w, r, item)
	})
}

// cncQueueRemoveHandler — DELETE /api/cnc/queue/{id}
func cncQueueRemoveHandler(registry *cnc.Registry) handleFunc {
	return withUser(func(w http.ResponseWriter, r *http.Request, d *data) (int, error) {
		if !d.authz().CanModify() {
			return http.StatusForbidden, nil
		}
		cd := d.cncapiDeps(registry)
		_, machineID, code, err := cd.ResolveStreamer(r.URL.Query().Get("machine_id"))
		if err != nil {
			return code, err
		}
		id := strings.TrimPrefix(r.URL.Path, "/api/cnc/queue/")
		code, err = cd.QueueRemove(machineID, id)
		if err != nil {
			return code, err
		}
		return renderJSON(w, r, map[string]bool{"removed": true})
	})
}

// cncQueueReorderHandler — PATCH /api/cnc/queue
func cncQueueReorderHandler(registry *cnc.Registry) handleFunc {
	return withUser(func(w http.ResponseWriter, r *http.Request, d *data) (int, error) {
		if !d.authz().CanModify() {
			return http.StatusForbidden, nil
		}
		cd := d.cncapiDeps(registry)
		_, machineID, code, err := cd.ResolveStreamer(r.URL.Query().Get("machine_id"))
		if err != nil {
			return code, err
		}
		req := struct {
			IDs []string `json:"ids"`
		}{}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			return http.StatusBadRequest, err
		}
		list, code, err := cd.QueueReorder(machineID, req.IDs)
		if err != nil {
			return code, err
		}
		return renderJSON(w, r, list)
	})
}
