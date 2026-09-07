package cncapi

// Shared logic behind /api/cnc/preflight and /api/cnc/auto-send.
// Mirrors http/cnc.go's cncPreflightHandler and
// http/cnc_autosend.go's cncAutoSendHandler.

import (
	"errors"
	"fmt"

	"github.com/filebrowser/filebrowser/v2/cnc"
)

// Preflight joins filePath's tool references against machineID's
// latest persisted tool-table dump. Read-only. Mirrors
// cncPreflightHandler.
func (d Deps) Preflight(filePath, machineID string) (*cnc.Preflight, int, error) {
	clean, err := CleanScopedPath(filePath)
	if err != nil {
		return nil, 400, err
	}
	absPath, err := d.Resolver.FullPath(clean)
	if err != nil {
		return nil, 400, err
	}
	_, resolvedID, status, err := d.ResolveStreamer(machineID)
	if err != nil {
		return nil, status, err
	}
	table, err := d.LatestToolTable(resolvedID)
	if err != nil {
		return nil, 400, err
	}
	pf, err := cnc.BuildPreflight(absPath, clean, resolvedID, table, d.CurrentSpindleTool(resolvedID))
	if err != nil {
		return nil, errToStatusDefault(err), err
	}
	return pf, 0, nil
}

// AutoSendRequest is the wire shape for POST /api/cnc/auto-send.
type AutoSendRequest struct {
	FilePath  string `json:"file_path"`
	MachineID string `json:"machine_id,omitempty"`
	Method    string `json:"method,omitempty"`
	QueueID   string `json:"queue_id,omitempty"`
}

// AutoSendResponse is the wire shape on success AND blocked. Mirrors
// http/cnc_autosend.go's autoSendResponse.
type AutoSendResponse struct {
	Started       bool           `json:"started"`
	JobID         string         `json:"job_id,omitempty"`
	BlockedReason string         `json:"blocked_reason,omitempty"`
	Preflight     *cnc.Preflight `json:"preflight,omitempty"`
}

// AutoSend runs preflight, evaluates the auto-send gate, and either
// starts the job or returns the block reason. status is 202 when the
// job starts, 0 (default 200) when blocked or when the caller should
// treat it as a plain success body, and a 4xx/5xx with a non-nil err
// otherwise. Mirrors cncAutoSendHandler.
func (d Deps) AutoSend(req AutoSendRequest, queryMachineID string) (AutoSendResponse, int, error) {
	if req.FilePath == "" {
		return AutoSendResponse{}, 400, fmt.Errorf("file_path required")
	}
	machineID := req.MachineID
	if machineID == "" {
		machineID = queryMachineID
	}
	streamer, resolvedID, status, err := d.ResolveStreamer(machineID)
	if err != nil {
		return AutoSendResponse{}, status, err
	}
	if ag, _, _, aerr := d.ResolveAggregator(machineID); aerr == nil {
		ag.Wake(0)
	}

	cfg := d.Store.Snapshot()
	machineCfg := FindMachine(cfg, resolvedID)
	if machineCfg == nil {
		return AutoSendResponse{}, 404, fmt.Errorf("no machine configured (id=%q)", machineID)
	}
	if !machineCfg.AutoSendEnabled {
		return AutoSendResponse{
			BlockedReason: "auto-send is not enabled for this machine (Settings → Machine)",
		}, 0, nil
	}

	clean, err := CleanScopedPath(req.FilePath)
	if err != nil {
		return AutoSendResponse{}, 400, err
	}
	absPath, err := d.Resolver.FullPath(clean)
	if err != nil {
		return AutoSendResponse{}, 400, err
	}

	// Auto-send requires a fresh table — without one we can't classify
	// tools, so refuse rather than send blind.
	table, terr := d.LatestToolTable(resolvedID)
	if terr != nil {
		return AutoSendResponse{}, 400, terr
	}

	pf, perr := cnc.BuildPreflight(absPath, clean, resolvedID, table, d.CurrentSpindleTool(resolvedID))
	if perr != nil {
		return AutoSendResponse{}, 400, fmt.Errorf("preflight failed: %w", perr)
	}
	if reason := AutoSendBlockReason(pf); reason != "" {
		return AutoSendResponse{BlockedReason: reason, Preflight: pf}, 0, nil
	}

	method := cnc.NormalizeSendMethod(req.Method)
	qs := d.Registry.Queues()
	if req.QueueID != "" && qs != nil {
		if _, qerr := qs.MarkSending(resolvedID, req.QueueID, string(method)); qerr == nil {
			streamer.EmitQueueSnapshot(qs.List(resolvedID))
		}
	}
	st, serr := streamer.Start(absPath, clean, method)
	if serr != nil {
		if req.QueueID != "" && qs != nil {
			qs.ClearInFlight(resolvedID)
			streamer.EmitQueueSnapshot(qs.List(resolvedID))
		}
		switch {
		case errors.Is(serr, cnc.ErrJobAlreadyRunning), errors.Is(serr, cnc.ErrRecoveryPending):
			return AutoSendResponse{}, 409, serr
		case errors.Is(serr, cnc.ErrConfigMissing):
			return AutoSendResponse{}, 400, serr
		default:
			return AutoSendResponse{}, errToStatusDefault(serr), serr
		}
	}
	return AutoSendResponse{Started: true, JobID: st.JobID, Preflight: pf}, 202, nil
}

// AutoSendBlockReason returns a non-empty string when preflight
// surfaces any condition that should keep the operator in the manual
// wizard loop. Empty string = clear to send. Mirrors
// http/cnc_autosend.go's autoSendBlockReason exactly (that function
// becomes a thin wrapper so http/cnc_autosend_test.go keeps testing
// the same code path under its original name).
func AutoSendBlockReason(pf *cnc.Preflight) string {
	if pf == nil {
		return "preflight unavailable"
	}
	if pf.TableMissing {
		return "no tool-table read on file — read the table first"
	}
	if pf.Summary.Missing > 0 {
		return fmt.Sprintf("%d tool(s) missing from the table", pf.Summary.Missing)
	}
	if pf.Summary.Empty > 0 {
		return fmt.Sprintf("%d tool(s) report empty pocket", pf.Summary.Empty)
	}
	if pf.Summary.Offline > 0 {
		return fmt.Sprintf("%d tool(s) errored on the last read", pf.Summary.Offline)
	}
	if pf.Summary.Warn > 0 {
		return fmt.Sprintf("%d tool(s) flagged warn (diameter drift / cutter-comp)", pf.Summary.Warn)
	}
	if pf.SpindleSwap {
		return "spindle swap pending — confirm starting tool in the wizard"
	}
	return ""
}
