package mcp

import (
	"context"
	"fmt"
	"time"

	mcpsdk "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/filebrowser/filebrowser/v2/cnc"
)

// ToolTableArgs is tool_table's input.
type ToolTableArgs struct {
	MachineID string `json:"machine_id,omitempty" jsonschema_description:"Machine ID to query. Omit to use the daemon's default (first configured) machine."`
	// Refresh requests a live read off the control instead of the
	// last persisted dump. Only attempted when the bridge link is
	// connected and no job is currently streaming; otherwise the
	// persisted dump is returned as-is with RefreshNote explaining why.
	Refresh bool `json:"refresh,omitempty" jsonschema_description:"Read the live tool table off the control instead of returning the last persisted dump. Only attempted when the machine is connected and not mid-job; a 30-slot read takes roughly 18 seconds."`
}

// ToolTableResult is tool_table's output. Table is nil only when the
// machine has never had its tool table read and no refresh happened —
// callers should treat that as "unknown", not "empty magazine".
type ToolTableResult struct {
	MachineID   string `json:"machine_id"`
	MachineName string `json:"machine_name,omitempty"`
	Connected   bool   `json:"connected"`
	// Refreshed is true when Table reflects a live read performed by
	// this call, false when it's the last persisted dump (or nil).
	Refreshed bool `json:"refreshed"`
	// RefreshNote explains why Refresh was requested but skipped, or
	// carries a partial-read error. Empty when Refresh wasn't
	// requested, or was requested and fully succeeded.
	RefreshNote string `json:"refresh_note,omitempty"`
	// Table carries every slot's geometry length/diameter, wear, the
	// pre-computed effective (geom+wear) values, CycleCount (the
	// per-tool life/probe counter, once a machine's macro range for it
	// is confirmed — see docs/TOOL_LIFE_RESEARCH.md), and ReadAt as
	// the "last read" timestamp. Pocket N holds tool N (carousel
	// semantics — see cnc/toollist.go).
	Table *cnc.ToolTable `json:"table,omitempty"`
}

func registerToolTable(s *server.MCPServer, d Deps) {
	tool := mcpsdk.NewTool("tool_table",
		mcpsdk.WithDescription(
			"Live tool-table pockets: T number, geometry length/diameter, wear, the tool-life/probe "+
				"counter where available, and when the table was last read. Set refresh=true to force a "+
				"fresh read off the control (only happens when the machine is connected and idle)."),
		mcpsdk.WithInputSchema[ToolTableArgs](),
		mcpsdk.WithReadOnlyHintAnnotation(true),
	)
	s.AddTool(tool, toolTableHandler(d))
}

// refreshSlotTimeout is how long a live ReadToolTable is allowed to
// run per requested slot, plus a flat startup allowance. Mirrors the
// ~0.6s/slot two-pass estimate documented on cnc.Streamer.ReadToolTable
// (worse case: every slot populated, both wear bases read).
const (
	refreshPerSlotBudget = 700 * time.Millisecond
	refreshFlatBudget    = 30 * time.Second
)

func toolTableHandler(d Deps) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		var args ToolTableArgs
		if err := req.BindArguments(&args); err != nil {
			return mcpsdk.NewToolResultError(err.Error()), nil
		}
		if d.Registry == nil {
			return mcpsdk.NewToolResultError("cncd: no registry configured"), nil
		}

		st, resolvedID := d.Registry.Streamer(args.MachineID)
		ag, _ := d.Registry.Aggregator(args.MachineID)
		if st == nil {
			return mcpsdk.NewToolResultError(
				fmt.Sprintf("no machine configured (id=%q)", args.MachineID)), nil
		}

		var name string
		slots := 0
		if d.FindMachine != nil {
			if m := d.FindMachine(resolvedID); m != nil {
				name = m.Name
				slots = m.EffectiveToolSlots()
			}
		}
		if slots <= 0 {
			slots = 30
		}

		var table *cnc.ToolTable
		if d.LatestToolTable != nil {
			if t, err := d.LatestToolTable(resolvedID); err == nil {
				table = t
			}
		}

		result := ToolTableResult{
			MachineID:   resolvedID,
			MachineName: name,
			Connected:   st.Alive(),
			Table:       table,
		}

		if args.Refresh {
			if ag != nil {
				ag.Wake(0)
			}
			switch {
			case st.IsRunning():
				result.RefreshNote = "not refreshed: a job is currently streaming to this machine"
			case !st.Alive():
				result.RefreshNote = "not refreshed: bridge link is not connected"
			default:
				timeout := time.Duration(slots)*refreshPerSlotBudget + refreshFlatBudget
				readCtx, cancel := context.WithTimeout(ctx, timeout)
				fresh, err := st.ReadToolTable(readCtx, slots)
				cancel()
				if fresh != nil {
					table = fresh
					result.Table = fresh
					result.Refreshed = true
				}
				if err != nil {
					result.RefreshNote = "refresh error: " + err.Error()
				}
			}
		}

		summary := toolTableSummary(result)
		return mcpsdk.NewToolResultStructured(result, summary), nil
	}
}

func toolTableSummary(r ToolTableResult) string {
	label := machineLabel(r.MachineID, r.MachineName)
	if r.Table == nil {
		return fmt.Sprintf("%s: no tool table has ever been read.", label)
	}
	populated := 0
	for _, s := range r.Table.Slots {
		if !s.Empty && len(s.Errors) == 0 {
			populated++
		}
	}
	state := "persisted dump"
	if r.Refreshed {
		state = "fresh read"
	}
	msg := fmt.Sprintf("%s: %s from %s — %d/%d slots populated.",
		label, state, r.Table.ReadAt.UTC().Format("2006-01-02 15:04:05Z"), populated, len(r.Table.Slots))
	if r.RefreshNote != "" {
		msg += " " + r.RefreshNote
	}
	return msg
}
