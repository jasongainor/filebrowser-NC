package cncd

// Run-kind override — POST/GET /api/cnc/run-kind.
//
// Jason's call on R&D/prove-out vs walk-away runs: log every run (the
// machine can't tell them apart, and prove-out time is still data),
// but classify each one with a kind — cnc.RunKindTrial (first
// article), cnc.RunKindProduction (walk-away), or cnc.RunKindRnd (no
// job). gmw-mes owns the DEFAULT classification rule (first run of a
// job+program-sha is trial, later runs are production; no GMW-JOB
// header at all is rnd) and the Carbon time mapping (production time
// -> machine time, trial time -> setup time). This daemon's only job
// here is to carry an explicit kind when it already knows one (the
// program's GMW-KIND header, cnc/program_identity.go) and to let the
// operator override the CURRENT run's kind when the default guess is
// wrong — e.g. the first run of a job turns out to actually be a
// walk-away production run, not a prove-out.
//
// Both routes act on the run cnc.Reporter currently has open for a
// machine; there is no history/list surface here, and no interaction
// with gmw-mes's own default-rule logic, which lives entirely on that
// side.

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/gorilla/mux"

	"github.com/filebrowser/filebrowser/v2/cnc"
)

// registerRunKind mounts POST/GET /api/cnc/run-kind onto r. Called
// once from NewRouter, alongside registerCNC/registerMCP/registerLogin.
func registerRunKind(r *mux.Router, d Deps) {
	api := r.PathPrefix("/api").Subrouter()
	cncRouter := api.PathPrefix("/cnc").Subrouter()
	cncRouter.HandleFunc("/run-kind", d.runKindSetHandler).Methods("POST")
	cncRouter.HandleFunc("/run-kind", d.runKindGetHandler).Methods("GET")
}

// runKindRequest is the wire shape for POST /api/cnc/run-kind.
// machine_id may also arrive as the usual ?machine_id= query param
// (checked when the body omits it), matching every other mutating
// route in this package (see preflightRequest's comment in
// router_cnc.go).
type runKindRequest struct {
	MachineID string `json:"machine_id,omitempty"`
	Kind      string `json:"kind"`
}

// runKindSetHandler serves POST /api/cnc/run-kind: the operator's
// override of the CURRENT run's kind. Modify-gated, same tier as
// /api/cnc/start|stop|attach. 400 for a missing/unrecognized kind,
// 404 for an unknown machine, 409 when the resolved machine has no
// run open right now (cnc.Reporter.SetKind's only failure mode).
func (d Deps) runKindSetHandler(w http.ResponseWriter, r *http.Request) {
	cd := d.cncapiDeps(r)
	if !cd.Authz.CanModify() {
		writeError(w, http.StatusForbidden, nil)
		return
	}
	var req runKindRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !cnc.ValidRunKind(req.Kind) {
		writeError(w, http.StatusBadRequest, fmt.Errorf(
			"kind must be one of: %s, %s, %s", cnc.RunKindTrial, cnc.RunKindProduction, cnc.RunKindRnd))
		return
	}
	machineID := req.MachineID
	if machineID == "" {
		machineID = r.URL.Query().Get("machine_id")
	}
	_, resolvedID, status, err := cd.ResolveStreamer(machineID)
	if err != nil {
		respond(w, status, nil, err)
		return
	}
	if !d.Registry.Reporter().SetKind(resolvedID, req.Kind) {
		writeError(w, http.StatusConflict, errors.New("no run open for this machine"))
		return
	}
	respond(w, 0, map[string]string{"machine_id": resolvedID, "kind": req.Kind}, nil)
}

// runKindGetHandler serves GET /api/cnc/run-kind?machine_id=: the
// resolved machine's currently open run, if any. Open reader, same
// posture as GET /api/cnc/status — no CanModify gate. "open": false
// (with empty kind/run_id) is a normal answer, not an error; only an
// unresolvable machine_id is a failure (404).
func (d Deps) runKindGetHandler(w http.ResponseWriter, r *http.Request) {
	cd := d.cncapiDeps(r)
	_, resolvedID, status, err := cd.ResolveStreamer(r.URL.Query().Get("machine_id"))
	if err != nil {
		respond(w, status, nil, err)
		return
	}
	kind, runID, open := d.Registry.Reporter().OpenRunInfo(resolvedID)
	respond(w, 0, map[string]any{
		"machine_id": resolvedID,
		"open":       open,
		"kind":       kind,
		"run_id":     runID,
	}, nil)
}
