package cncd

// registerCNC mounts the full /api/cnc/* surface (beyond the
// state/qcode/stream trio already wired directly in router.go) plus
// admin CRUD on /api/cnc/displays and the per-machine tool-list view
// at /api/machines/{id}/toollist. Every handler here is a thin
// wrapper: the actual logic lives in cncapi (handlers_*.go), shared
// verbatim with filebrowser's http/cnc*.go so the two servers answer
// identically for the same request.
//
// Gating mirrors filebrowser's admin/modify/plain-user three-tier
// model, collapsed onto cncd's two-tier bearer (docs/CNCD.md):
//   - fbhttp withAdmin routes  -> Authz.IsAdmin()
//   - fbhttp CanModify() gates -> Authz.CanModify()
//   - fbhttp withUser-only     -> open (LAN-permissive, same posture
//     as GET /api/files and GET /api/displays/{id} with no token set)
//
// On cncd today IsAdmin() and CanModify() are the same bit (a
// matching machine-token bearer gets both), but the calls are kept
// distinct so a future session-based Authz (docs/CNCD.md's "not here
// yet") can split them without touching this file.
import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/gorilla/mux"

	"github.com/filebrowser/filebrowser/v2/cncapi"
	"github.com/filebrowser/filebrowser/v2/settings"
)

// cncapiDeps builds the cncapi.Deps for one request: a resolver
// jailed to d.Root, and an Authz derived from the request's bearer
// against the currently configured machine token.
func (d Deps) cncapiDeps(r *http.Request) cncapi.Deps {
	return cncapi.Deps{
		Registry: d.Registry,
		Resolver: cncapi.NewRootResolver(d.Root),
		Authz:    authzFor(r, d.Config.Snapshot().MachineToken),
		Store:    d.Config,
	}
}

// decodeJSON reads and decodes the request body into v, writing a 400
// and returning false on failure.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return false
	}
	return true
}

// respond renders body as JSON on success, or writes the error status
// on failure. status is the intended success code (0 means "200,
// nothing special" — e.g. 202 Accepted for auto-send) when err is
// nil, or the failure code to send when err is non-nil.
func respond(w http.ResponseWriter, status int, body any, err error) {
	if err != nil {
		if status == 0 {
			status = http.StatusInternalServerError
		}
		writeError(w, status, err)
		return
	}
	if status != 0 && status != http.StatusOK {
		w.WriteHeader(status)
	}
	_ = renderJSON(w, body)
}

func queryInt(r *http.Request, key string) (int, bool, error) {
	q := r.URL.Query().Get(key)
	if q == "" {
		return 0, false, nil
	}
	n, err := strconv.Atoi(q)
	if err != nil {
		return 0, true, err
	}
	return n, true, nil
}

// registerCNC mounts every route this file owns onto r. Called once
// from NewRouter.
func registerCNC(r *mux.Router, d Deps) {
	api := r.PathPrefix("/api").Subrouter()
	cncRouter := api.PathPrefix("/cnc").Subrouter()

	// ---- queue ----
	cncRouter.HandleFunc("/queue", d.queueListHandler).Methods("GET")
	cncRouter.HandleFunc("/queue", d.queueAddHandler).Methods("POST")
	cncRouter.HandleFunc("/queue", d.queueReorderHandler).Methods("PATCH")
	cncRouter.HandleFunc("/queue/{id}", d.queueRemoveHandler).Methods("DELETE")
	cncRouter.HandleFunc("/queue/{id}/promote", d.queuePromoteHandler).Methods("POST")

	// ---- run: status/start/stop/attach/detach ----
	cncRouter.HandleFunc("/status", d.cncStatusHandler).Methods("GET")
	cncRouter.HandleFunc("/start", d.cncStartHandler).Methods("POST")
	cncRouter.HandleFunc("/stop", d.cncStopHandler).Methods("POST")
	cncRouter.HandleFunc("/attach", d.cncAttachHandler).Methods("POST")
	cncRouter.HandleFunc("/attach", d.cncDetachHandler).Methods("DELETE")

	// ---- preflight + auto-send ----
	cncRouter.HandleFunc("/preflight", d.cncPreflightHandler).Methods("POST")
	cncRouter.HandleFunc("/auto-send", d.cncAutoSendHandler).Methods("POST")

	// ---- tool table ----
	cncRouter.HandleFunc("/tool-table", d.toolTableReadHandler).Methods("POST")
	cncRouter.HandleFunc("/tool-table", d.toolTableLatestHandler).Methods("GET")
	cncRouter.HandleFunc("/tool-table/history", d.toolTableHistoryHandler).Methods("GET")
	cncRouter.HandleFunc("/tool-table/edit", d.toolTableEditHandler).Methods("POST")
	api.HandleFunc("/machines/{id}/toollist", d.machineToolListHandler).Methods("GET")
	cncRouter.HandleFunc("/tool-library", d.toolLibraryGetHandler).Methods("GET")
	cncRouter.HandleFunc("/tool-library", d.toolLibraryPutHandler).Methods("PUT")

	// ---- machines + settings ----
	cncRouter.HandleFunc("/machines", d.machinesListHandler).Methods("GET")
	cncRouter.HandleFunc("/settings", d.settingsGetHandler).Methods("GET")
	cncRouter.HandleFunc("/settings", d.settingsPutHandler).Methods("PUT")
	cncRouter.HandleFunc("/settings/token", d.settingsTokenHandler).Methods("POST")

	// ---- jobs / codes / host-stats / recovery / displays ----
	cncRouter.HandleFunc("/jobs", d.jobsListHandler).Methods("GET")
	cncRouter.HandleFunc("/jobs/stats", d.jobsStatsHandler).Methods("GET")
	cncRouter.HandleFunc("/codes/lookup", d.codesLookupHandler).Methods("GET")
	cncRouter.HandleFunc("/codes/search", d.codesSearchHandler).Methods("GET")
	cncRouter.HandleFunc("/host-stats", d.hostStatsHandler).Methods("GET")
	cncRouter.HandleFunc("/recovery/ack", d.recoveryAckHandler).Methods("POST")
	cncRouter.HandleFunc("/displays", d.displaysListHandler).Methods("GET")
	cncRouter.HandleFunc("/displays", d.displaysCreateHandler).Methods("POST")
	cncRouter.HandleFunc("/displays/{id}", d.displaysUpdateHandler).Methods("PUT")
	cncRouter.HandleFunc("/displays/{id}", d.displaysDeleteHandler).Methods("DELETE")
}

// ---------------------------------------------------------------
// queue
// ---------------------------------------------------------------

func (d Deps) queueListHandler(w http.ResponseWriter, r *http.Request) {
	cd := d.cncapiDeps(r)
	_, resolvedID, status, err := cd.ResolveStreamer(r.URL.Query().Get("machine_id"))
	if err != nil {
		respond(w, status, nil, err)
		return
	}
	respond(w, 0, cd.QueueList(resolvedID), nil)
}

func (d Deps) queueAddHandler(w http.ResponseWriter, r *http.Request) {
	cd := d.cncapiDeps(r)
	if !cd.Authz.CanModify() {
		writeError(w, http.StatusForbidden, nil)
		return
	}
	var req cncapi.QueueAddRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	item, status, err := cd.QueueAdd(req, r.URL.Query().Get("machine_id"))
	respond(w, status, item, err)
}

func (d Deps) queueRemoveHandler(w http.ResponseWriter, r *http.Request) {
	cd := d.cncapiDeps(r)
	if !cd.Authz.CanModify() {
		writeError(w, http.StatusForbidden, nil)
		return
	}
	machineID := r.URL.Query().Get("machine_id")
	_, resolvedID, status, err := cd.ResolveStreamer(machineID)
	if err != nil {
		respond(w, status, nil, err)
		return
	}
	id := mux.Vars(r)["id"]
	status, err = cd.QueueRemove(resolvedID, id)
	respond(w, status, map[string]bool{"removed": true}, err)
}

func (d Deps) queueReorderHandler(w http.ResponseWriter, r *http.Request) {
	cd := d.cncapiDeps(r)
	if !cd.Authz.CanModify() {
		writeError(w, http.StatusForbidden, nil)
		return
	}
	machineID := r.URL.Query().Get("machine_id")
	_, resolvedID, status, err := cd.ResolveStreamer(machineID)
	if err != nil {
		respond(w, status, nil, err)
		return
	}
	var req struct {
		IDs []string `json:"ids"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	list, status, err := cd.QueueReorder(resolvedID, req.IDs)
	respond(w, status, list, err)
}

func (d Deps) queuePromoteHandler(w http.ResponseWriter, r *http.Request) {
	cd := d.cncapiDeps(r)
	if !cd.Authz.CanModify() {
		writeError(w, http.StatusForbidden, nil)
		return
	}
	machineID := r.URL.Query().Get("machine_id")
	_, resolvedID, status, err := cd.ResolveStreamer(machineID)
	if err != nil {
		respond(w, status, nil, err)
		return
	}
	id := mux.Vars(r)["id"]
	item, status, err := cd.QueuePromote(resolvedID, id)
	respond(w, status, item, err)
}

// ---------------------------------------------------------------
// status / start / stop / attach / detach
// ---------------------------------------------------------------

func (d Deps) cncStatusHandler(w http.ResponseWriter, r *http.Request) {
	cd := d.cncapiDeps(r)
	body, status, err := cd.Status(r.URL.Query().Get("machine_id"), "/api/files?path=")
	respond(w, status, body, err)
}

func (d Deps) cncStartHandler(w http.ResponseWriter, r *http.Request) {
	cd := d.cncapiDeps(r)
	if !cd.Authz.CanModify() {
		writeError(w, http.StatusForbidden, nil)
		return
	}
	var req cncapi.StartRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	jobID, status, err := cd.Start(req, r.URL.Query().Get("machine_id"))
	respond(w, status, map[string]string{"job_id": jobID}, err)
}

func (d Deps) cncStopHandler(w http.ResponseWriter, r *http.Request) {
	cd := d.cncapiDeps(r)
	if !cd.Authz.CanModify() {
		writeError(w, http.StatusForbidden, nil)
		return
	}
	stopped, status, err := cd.Stop(r.URL.Query().Get("machine_id"))
	respond(w, status, map[string]bool{"stopped": stopped}, err)
}

func (d Deps) cncAttachHandler(w http.ResponseWriter, r *http.Request) {
	cd := d.cncapiDeps(r)
	if !cd.Authz.CanModify() {
		writeError(w, http.StatusForbidden, nil)
		return
	}
	var req cncapi.AttachRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	st, status, err := cd.Attach(req, r.URL.Query().Get("machine_id"))
	respond(w, status, st, err)
}

func (d Deps) cncDetachHandler(w http.ResponseWriter, r *http.Request) {
	cd := d.cncapiDeps(r)
	if !cd.Authz.CanModify() {
		writeError(w, http.StatusForbidden, nil)
		return
	}
	st, status, err := cd.Detach(r.URL.Query().Get("machine_id"))
	respond(w, status, st, err)
}

// ---------------------------------------------------------------
// preflight / auto-send
// ---------------------------------------------------------------

// preflightRequest is the wire shape for POST /api/cnc/preflight on
// cncd. Unlike filebrowser's GET+query-param /api/cnc/preflight, this
// is a POST with a JSON body — matching auto-send's shape since cncd
// has no UI-driven query-string convention to stay compatible with.
type preflightRequest struct {
	FilePath  string `json:"file_path"`
	MachineID string `json:"machine_id,omitempty"`
}

func (d Deps) cncPreflightHandler(w http.ResponseWriter, r *http.Request) {
	cd := d.cncapiDeps(r)
	if !cd.Authz.CanModify() {
		writeError(w, http.StatusForbidden, nil)
		return
	}
	var req preflightRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	machineID := req.MachineID
	if machineID == "" {
		machineID = r.URL.Query().Get("machine_id")
	}
	pf, status, err := cd.Preflight(req.FilePath, machineID)
	respond(w, status, pf, err)
}

func (d Deps) cncAutoSendHandler(w http.ResponseWriter, r *http.Request) {
	cd := d.cncapiDeps(r)
	if !cd.Authz.CanModify() {
		writeError(w, http.StatusForbidden, nil)
		return
	}
	var req cncapi.AutoSendRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	body, status, err := cd.AutoSend(req, r.URL.Query().Get("machine_id"))
	respond(w, status, body, err)
}

// ---------------------------------------------------------------
// tool table
// ---------------------------------------------------------------

func (d Deps) toolTableReadHandler(w http.ResponseWriter, r *http.Request) {
	cd := d.cncapiDeps(r)
	if !cd.Authz.IsAdmin() {
		writeError(w, http.StatusForbidden, nil)
		return
	}
	machineID := r.URL.Query().Get("machine_id")
	slots := cncapi.DefaultToolSlotsForMachine(cd.Store.Snapshot(), machineID)
	if n, present, err := queryInt(r, "slots"); present {
		if err != nil || n < 1 || n > 200 {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		slots = n
	}
	// Worst case, a fully-loaded toolchanger at the lowest baud: allow
	// 15 minutes, matching cncToolTableReadHandler.
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Minute)
	defer cancel()
	env, status, err := cd.ToolTableReadLive(ctx, machineID, slots)
	respond(w, status, env, err)
}

func (d Deps) toolTableLatestHandler(w http.ResponseWriter, r *http.Request) {
	cd := d.cncapiDeps(r)
	machineID := r.URL.Query().Get("machine_id")
	tbl, found, status, err := cd.ToolTableLatest(machineID)
	if err != nil {
		respond(w, status, nil, err)
		return
	}
	if !found {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	_ = renderJSON(w, map[string]any{"table": tbl})
}

func (d Deps) toolTableHistoryHandler(w http.ResponseWriter, r *http.Request) {
	cd := d.cncapiDeps(r)
	body, status, err := cd.ToolTableHistory(r.URL.Query().Get("machine_id"), toolTableShareDir)
	respond(w, status, body, err)
}

func (d Deps) toolTableEditHandler(w http.ResponseWriter, r *http.Request) {
	cd := d.cncapiDeps(r)
	if !cd.Authz.CanModify() {
		writeError(w, http.StatusForbidden, nil)
		return
	}
	var req cncapi.ToolTableEditRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	tbl, status, err := cd.ToolTableEdit(req, r.URL.Query().Get("machine_id"))
	respond(w, status, map[string]any{"table": tbl}, err)
}

func (d Deps) machineToolListHandler(w http.ResponseWriter, r *http.Request) {
	cd := d.cncapiDeps(r)
	machineID := mux.Vars(r)["id"]
	if machineID == "" {
		writeError(w, http.StatusBadRequest, nil)
		return
	}
	list, err := cd.BuildMachineToolList(machineID)
	if err != nil {
		status := http.StatusInternalServerError
		if err == cncapi.ErrMachineNotFound {
			status = http.StatusNotFound
		}
		writeError(w, status, err)
		return
	}
	_ = renderJSON(w, list)
}

func (d Deps) toolLibraryGetHandler(w http.ResponseWriter, r *http.Request) {
	cd := d.cncapiDeps(r)
	_ = renderJSON(w, cd.ToolLibraryGet())
}

func (d Deps) toolLibraryPutHandler(w http.ResponseWriter, r *http.Request) {
	cd := d.cncapiDeps(r)
	if !cd.Authz.IsAdmin() {
		writeError(w, http.StatusForbidden, nil)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, cncapi.ToolLibraryUploadCap)
	buf, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	body, status, err := cd.ToolLibraryPut(buf)
	respond(w, status, body, err)
}

// ---------------------------------------------------------------
// machines / settings
// ---------------------------------------------------------------

func (d Deps) machinesListHandler(w http.ResponseWriter, r *http.Request) {
	cd := d.cncapiDeps(r)
	_ = renderJSON(w, cd.MachinesList())
}

// settingsGetHandler returns the full settings.Cnc document verbatim
// — unlike filebrowser's /api/cnc/settings (which has a narrower,
// legacy-compatible wire shape, cncSettingsBody), cncd has no
// pre-multi-machine UI to stay compatible with, so it just round-trips
// the whole struct: machines (incl. serial), displays, discord,
// machineToken, and anything added to settings.Cnc later.
func (d Deps) settingsGetHandler(w http.ResponseWriter, r *http.Request) {
	cd := d.cncapiDeps(r)
	if !cd.Authz.IsAdmin() {
		writeError(w, http.StatusForbidden, nil)
		return
	}
	_ = renderJSON(w, cd.Store.Snapshot())
}

// settingsPutHandler replaces the settings.Cnc document. Machines get
// the same validation as filebrowser's (cncapi.NormalizeMachines);
// every other field is trusted as decoded — settings.Cnc is treated
// opaquely so a field added later doesn't need a matching change here.
func (d Deps) settingsPutHandler(w http.ResponseWriter, r *http.Request) {
	cd := d.cncapiDeps(r)
	if !cd.Authz.IsAdmin() {
		writeError(w, http.StatusForbidden, nil)
		return
	}
	var req settings.Cnc
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := cd.SettingsUpdateMachines(req.Machines); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := cd.Store.Update(func(c *settings.Cnc) error {
		// Machines already validated + saved by SettingsUpdateMachines
		// above; carry the rest of the document across verbatim.
		c.MachineToken = req.MachineToken
		c.Discord = req.Discord
		c.Displays = req.Displays
		c.BaselinePollSeconds = req.BaselinePollSeconds
		return nil
	}); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	_ = renderJSON(w, cd.Store.Snapshot())
}

func (d Deps) settingsTokenHandler(w http.ResponseWriter, r *http.Request) {
	cd := d.cncapiDeps(r)
	if !cd.Authz.IsAdmin() {
		writeError(w, http.StatusForbidden, nil)
		return
	}
	tok, err := cd.RegenerateMachineToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	_ = renderJSON(w, map[string]string{"machineToken": tok})
}

// ---------------------------------------------------------------
// jobs / codes / host-stats / recovery / displays
// ---------------------------------------------------------------

func (d Deps) jobsListHandler(w http.ResponseWriter, r *http.Request) {
	cd := d.cncapiDeps(r)
	limit := 0
	if n, present, err := queryInt(r, "limit"); present && err == nil {
		limit = n
	}
	body, status, err := cd.JobsList(r.URL.Query().Get("machine_id"), limit)
	respond(w, status, body, err)
}

func (d Deps) jobsStatsHandler(w http.ResponseWriter, r *http.Request) {
	cd := d.cncapiDeps(r)
	days := -1
	if n, present, err := queryInt(r, "days"); present && err == nil && n >= 0 {
		days = n
	}
	stats, status, err := cd.JobsStats(r.URL.Query().Get("machine_id"), days)
	respond(w, status, stats, err)
}

func (d Deps) codesLookupHandler(w http.ResponseWriter, r *http.Request) {
	n, _, err := queryInt(r, "number")
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	_ = renderJSON(w, cncapi.CodeLookup(r.URL.Query().Get("kind"), n))
}

func (d Deps) codesSearchHandler(w http.ResponseWriter, r *http.Request) {
	limit, _, _ := queryInt(r, "limit")
	_ = renderJSON(w, cncapi.CodeSearch(r.URL.Query().Get("kind"), r.URL.Query().Get("q"), limit))
}

func (d Deps) hostStatsHandler(w http.ResponseWriter, r *http.Request) {
	_ = renderJSON(w, cncapi.HostStats())
}

func (d Deps) recoveryAckHandler(w http.ResponseWriter, r *http.Request) {
	cd := d.cncapiDeps(r)
	if !cd.Authz.CanModify() {
		writeError(w, http.StatusForbidden, nil)
		return
	}
	status, err := cd.RecoveryAck(r.URL.Query().Get("machine_id"))
	respond(w, status, map[string]bool{"acknowledged": true}, err)
}

func (d Deps) displaysListHandler(w http.ResponseWriter, r *http.Request) {
	cd := d.cncapiDeps(r)
	if !cd.Authz.IsAdmin() {
		writeError(w, http.StatusForbidden, nil)
		return
	}
	_ = renderJSON(w, map[string]any{"displays": cd.DisplaysList()})
}

func (d Deps) displaysCreateHandler(w http.ResponseWriter, r *http.Request) {
	cd := d.cncapiDeps(r)
	if !cd.Authz.IsAdmin() {
		writeError(w, http.StatusForbidden, nil)
		return
	}
	var req settings.Display
	if !decodeJSON(w, r, &req) {
		return
	}
	disp, status, err := cd.DisplaysCreate(req, cncapi.NewDisplayID())
	respond(w, status, disp, err)
}

func (d Deps) displaysUpdateHandler(w http.ResponseWriter, r *http.Request) {
	cd := d.cncapiDeps(r)
	if !cd.Authz.IsAdmin() {
		writeError(w, http.StatusForbidden, nil)
		return
	}
	var req settings.Display
	if !decodeJSON(w, r, &req) {
		return
	}
	disp, status, err := cd.DisplaysUpdate(mux.Vars(r)["id"], req)
	respond(w, status, disp, err)
}

func (d Deps) displaysDeleteHandler(w http.ResponseWriter, r *http.Request) {
	cd := d.cncapiDeps(r)
	if !cd.Authz.IsAdmin() {
		writeError(w, http.StatusForbidden, nil)
		return
	}
	status, err := cd.DisplaysDelete(mux.Vars(r)["id"])
	if err != nil {
		respond(w, status, nil, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}
