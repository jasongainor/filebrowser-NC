package cncapi

// Shared logic behind /api/cnc/status, /api/cnc/start, /api/cnc/stop,
// and /api/cnc/attach (POST + DELETE). Mirrors http/cnc.go's
// cncStatusHandler/cncStartHandler/cncStopHandler and
// http/cnc_attach.go.

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/filebrowser/filebrowser/v2/cnc"
)

// StatusBody is the wire shape for GET /api/cnc/status: *cnc.Status
// plus the machine it resolved to and (when a file is attached or
// running) a browsable URL for it. Mirrors the anonymous struct in
// cncStatusHandler.
type StatusBody struct {
	*cnc.Status
	MachineID string `json:"machine_id"`
	FileURL   string `json:"file_url,omitempty"`
}

// Status builds the GET /api/cnc/status body for machineID (empty =
// registry default). fileURLPrefix is the host's URL prefix for a
// browsable file link — "/files" on filebrowser (http/cnc.go),
// "/api/files?path=" on cncd; pass "" to omit FileURL entirely.
func (d Deps) Status(machineID, fileURLPrefix string) (StatusBody, int, error) {
	st, resolvedID, status, err := d.ResolveStreamer(machineID)
	if err != nil {
		return StatusBody{}, status, err
	}
	s := st.Status()
	body := StatusBody{Status: s, MachineID: resolvedID}
	if s.FilePath != "" && fileURLPrefix != "" {
		body.FileURL = fileURLPrefix + ensureLeading(s.FilePath)
	}
	return body, 0, nil
}

// StartRequest is the wire shape for POST /api/cnc/start.
type StartRequest struct {
	FilePath  string `json:"file_path"`
	MachineID string `json:"machine_id,omitempty"`
	Method    string `json:"method,omitempty"`
	QueueID   string `json:"queue_id,omitempty"`
}

// Start validates the request, applies the RequirePreflight gate, and
// starts the job. Mirrors cncStartHandler's body exactly, including
// the queue-row in-flight bookkeeping on both the happy and error
// paths.
func (d Deps) Start(req StartRequest, queryMachineID string) (jobID string, status int, err error) {
	if req.FilePath == "" {
		return "", 400, fmt.Errorf("file_path required")
	}
	machineID := req.MachineID
	if machineID == "" {
		machineID = queryMachineID
	}
	streamer, resolvedID, status, err := d.ResolveStreamer(machineID)
	if err != nil {
		return "", status, err
	}
	if ag, _, _, aerr := d.ResolveAggregator(machineID); aerr == nil {
		ag.Wake(0)
	}

	clean, err := CleanScopedPath(req.FilePath)
	if err != nil {
		return "", 400, err
	}
	absPath, err := d.Resolver.FullPath(clean)
	if err != nil {
		return "", 400, err
	}

	cfg := d.Store.Snapshot()
	machineCfg := FindMachine(cfg, resolvedID)
	if machineCfg != nil && machineCfg.RequirePreflight {
		table, _ := d.LatestToolTable(resolvedID)
		pf, perr := cnc.BuildPreflight(absPath, clean, resolvedID, table, d.CurrentSpindleTool(resolvedID))
		if perr == nil {
			if pf.TableMissing {
				return "", 409, errors.New(
					"preflight required: no tool-table read on file for this machine — read the table on /machine first")
			}
			if pf.Summary.Missing > 0 || pf.Summary.Empty > 0 {
				return "", 409, fmt.Errorf(
					"preflight required: %d missing, %d empty pocket — fix on the controller or disable Require preflight in Settings → Machine",
					pf.Summary.Missing, pf.Summary.Empty)
			}
		}
		// If BuildPreflight itself failed, fall through to Start's own
		// error path rather than wedging sends behind a gate that
		// can't evaluate.
	}

	method := cnc.NormalizeSendMethod(req.Method)
	qs := d.Registry.Queues()
	if req.QueueID != "" && qs != nil {
		if _, qerr := qs.MarkSending(resolvedID, req.QueueID, string(method)); qerr == nil {
			streamer.EmitQueueSnapshot(qs.List(resolvedID))
		}
	}
	st, serr := streamer.Start(absPath, clean, method)
	demoteQueue := func() {
		if req.QueueID != "" && qs != nil {
			qs.ClearInFlight(resolvedID)
			streamer.EmitQueueSnapshot(qs.List(resolvedID))
		}
	}
	switch {
	case errors.Is(serr, cnc.ErrJobAlreadyRunning), errors.Is(serr, cnc.ErrRecoveryPending):
		return "", 409, serr
	case errors.Is(serr, cnc.ErrConfigMissing):
		demoteQueue()
		return "", 400, serr
	case serr != nil:
		demoteQueue()
		return "", errToStatusDefault(serr), serr
	}
	return st.JobID, 0, nil
}

// Stop stops the current job on machineID (if any) and clears any
// in-flight queue row. Mirrors cncStopHandler.
func (d Deps) Stop(machineID string) (stopped bool, status int, err error) {
	st, resolvedID, status, err := d.ResolveStreamer(machineID)
	if err != nil {
		return false, status, err
	}
	stopped = st.Stop()
	if qs := d.Registry.Queues(); qs != nil {
		qs.ClearInFlight(resolvedID)
		st.EmitQueueSnapshot(qs.List(resolvedID))
	}
	return stopped, 0, nil
}

// AttachRequest is the wire shape for POST /api/cnc/attach.
type AttachRequest struct {
	FilePath  string `json:"file_path"`
	MachineID string `json:"machine_id,omitempty"`
	Source    string `json:"source,omitempty"`
}

// Attach validates req, marks it on the streamer, and broadcasts the
// new status. Mirrors cncAttachHandler.
func (d Deps) Attach(req AttachRequest, queryMachineID string) (*cnc.Status, int, error) {
	if req.FilePath == "" {
		return nil, 400, fmt.Errorf("file_path required")
	}
	machineID := req.MachineID
	if machineID == "" {
		machineID = queryMachineID
	}
	streamer, _, status, err := d.ResolveStreamer(machineID)
	if err != nil {
		return nil, status, err
	}
	clean, err := CleanScopedPath(req.FilePath)
	if err != nil {
		return nil, 400, err
	}
	abs, err := d.Resolver.FullPath(clean)
	if err != nil {
		return nil, 400, err
	}
	if _, err := os.Stat(abs); err != nil {
		return nil, 404, fmt.Errorf("file not in scope: %s", clean)
	}
	source := strings.ToLower(strings.TrimSpace(req.Source))
	if source != "manual" && source != "auto" {
		source = "manual"
	}
	if err := streamer.Attach(clean, source); err != nil {
		return nil, 409, err
	}
	streamer.EmitStatus()
	return streamer.Status(), 0, nil
}

// Detach clears any attachment on machineID's streamer and broadcasts
// the new status. Mirrors cncDetachHandler.
func (d Deps) Detach(machineID string) (*cnc.Status, int, error) {
	streamer, _, status, err := d.ResolveStreamer(machineID)
	if err != nil {
		return nil, status, err
	}
	if streamer.Detach() {
		streamer.EmitStatus()
	}
	return streamer.Status(), 0, nil
}
