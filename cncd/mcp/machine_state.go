package mcp

import (
	"context"
	"fmt"

	mcpsdk "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/filebrowser/filebrowser/v2/cnc"
)

// MachineStateArgs is machine_state's input.
type MachineStateArgs struct {
	MachineID string `json:"machine_id,omitempty" jsonschema_description:"Machine ID to query. Omit to use the daemon's default (first configured) machine."`
}

// MachineStateResult is machine_state's output: the aggregator's live
// metric snapshot (mode, current tool, status_combined — which
// carries the running program number and parts count — spindle RPM,
// machine/work positions, G54 offsets) plus link/job flags a CAM
// session shouldn't have to derive itself from raw Q-code strings.
type MachineStateResult struct {
	MachineID   string                 `json:"machine_id"`
	MachineName string                 `json:"machine_name,omitempty"`
	// Connected reports whether the RS-232<->TCP bridge link is
	// alive — see cnc.Streamer.Alive(). false does not necessarily
	// mean the machine is off; it means the daemon has no fresh
	// baseline poll to show.
	Connected bool `json:"connected"`
	// JobRunning is true while THIS daemon is actively streaming a
	// program to the control. A program started from the Haas panel
	// itself (SD card, USB, MDI) does not set this — check the
	// status_combined metric's parsed "status" field for that.
	JobRunning bool `json:"job_running"`
	// Metrics is the same shape GET /api/cnc/state returns: one entry
	// per polled Q-code field, keyed by metric name (mode, tool,
	// last_cycle, parts, status_combined, current_block,
	// spindle_actual, spindle_cmd, pos_x/y/z, work_x/y/z, g54_x/y/z).
	Metrics map[string]*cnc.Metric `json:"metrics"`
}

func registerMachineState(s *server.MCPServer, d Deps) {
	tool := mcpsdk.NewTool("machine_state",
		mcpsdk.WithDescription(
			"Current machine state: mode, current tool, running program + parts count, "+
				"spindle RPM, machine/work positions, and whether the bridge link is connected. "+
				"Read-only, cheap, safe to call before programming to sanity-check the box is reachable."),
		mcpsdk.WithInputSchema[MachineStateArgs](),
		mcpsdk.WithReadOnlyHintAnnotation(true),
	)
	s.AddTool(tool, machineStateHandler(d))
}

func machineStateHandler(d Deps) server.ToolHandlerFunc {
	return func(_ context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		var args MachineStateArgs
		if err := req.BindArguments(&args); err != nil {
			return mcpsdk.NewToolResultError(err.Error()), nil
		}
		if d.Registry == nil {
			return mcpsdk.NewToolResultError("cncd: no registry configured"), nil
		}

		st, resolvedID := d.Registry.Streamer(args.MachineID)
		ag, _ := d.Registry.Aggregator(args.MachineID)
		if st == nil || ag == nil {
			return mcpsdk.NewToolResultError(
				fmt.Sprintf("no machine configured (id=%q)", args.MachineID)), nil
		}
		// Same wake-on-read behavior as GET /api/cnc/state: a caller
		// asking for state implies someone is watching, so bump the
		// aggregator to its full-rate polling window.
		ag.Wake(0)

		var name string
		if d.FindMachine != nil {
			if m := d.FindMachine(resolvedID); m != nil {
				name = m.Name
			}
		}

		result := MachineStateResult{
			MachineID:   resolvedID,
			MachineName: name,
			Connected:   st.Alive(),
			JobRunning:  st.IsRunning(),
			Metrics:     ag.Snapshot(),
		}
		summary := fmt.Sprintf("%s: connected=%v, job_running=%v, %d metrics reporting.",
			machineLabel(resolvedID, name), result.Connected, result.JobRunning, len(result.Metrics))
		return mcpsdk.NewToolResultStructured(result, summary), nil
	}
}

// machineLabel renders "Name (id)" or just "id" when no name is set —
// used in the plain-text fallback content every tool result carries
// alongside its structured payload.
func machineLabel(id, name string) string {
	if name == "" {
		return id
	}
	return fmt.Sprintf("%s (%s)", name, id)
}
