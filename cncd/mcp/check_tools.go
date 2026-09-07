package mcp

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	mcpsdk "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/filebrowser/filebrowser/v2/cnc"
)

// CheckToolsSimpleTool is one entry of check_tools' simplified tools[]
// input — for a CAM session that hasn't assembled a full .gmw.json
// sidecar document yet, or just wants to check a handful of tools.
type CheckToolsSimpleTool struct {
	T              int     `json:"t" jsonschema_description:"Tool number (T-code)."`
	Diameter       float64 `json:"diameter" jsonschema_description:"Cutter diameter."`
	FluteLength    float64 `json:"flute_length,omitempty" jsonschema_description:"Flute length — limits cut depth before the flute runs out."`
	StickoutLength float64 `json:"stickout_length,omitempty" jsonschema_description:"Tool length exposed below the holder — limits depth before the holder itself collides with the part or a fixture."`
	MaxDepth       float64 `json:"max_depth,omitempty" jsonschema_description:"Deepest the toolpath cuts with this tool, as a positive number below stock top."`
}

// CheckToolsArgs is check_tools' input: exactly one of Sidecar or
// Tools must be set.
type CheckToolsArgs struct {
	MachineID string `json:"machine_id,omitempty" jsonschema_description:"Machine ID to check against. Omit to use the daemon's default (first configured) machine."`
	// Sidecar is a full .gmw.json document — see the program_identity
	// resource for the schema this mirrors (cnc.Sidecar).
	Sidecar *cnc.Sidecar `json:"sidecar,omitempty" jsonschema_description:"A full .gmw.json sidecar document (see the program_identity resource for the schema). Use this OR tools, not both."`
	// Tools is the simplified alternative to Sidecar.
	Tools []CheckToolsSimpleTool `json:"tools,omitempty" jsonschema_description:"Simplified tool list, used instead of sidecar when you don't have a full sidecar document yet. Use this OR sidecar, not both."`
	// Clearance overrides cnc.DefaultClearanceMargin when > 0.
	Clearance float64 `json:"clearance,omitempty" jsonschema_description:"Clearance margin added to each tool's max_depth to get its required reach. Defaults to 0.1 (cnc.DefaultClearanceMargin) when omitted."`
}

// CheckToolsResult is check_tools' output.
type CheckToolsResult struct {
	MachineID   string `json:"machine_id"`
	TableFound  bool   `json:"table_found"`
	TableReadAt string `json:"table_read_at,omitempty"`
	// Report is cnc.ReconcileTools' full per-tool result: pocket
	// status (match/moved/missing), diameter warnings, and length-
	// insufficient flags with the numbers behind each.
	Report *cnc.ToolReconcileReport `json:"report"`
	// Summary is a one-paragraph, plain-English readout a CAM session
	// can act on directly without walking Report itself.
	Summary string `json:"summary"`
}

func registerCheckTools(s *server.MCPServer, d Deps) {
	tool := mcpsdk.NewTool("check_tools",
		mcpsdk.WithDescription(
			"Reconcile a program's tool list — either a full .gmw.json sidecar or a simplified "+
				"tools[] array — against the machine's tool table. Reports, per tool, whether it's "+
				"loaded in the expected pocket, moved to another pocket, or missing entirely; whether "+
				"its diameter matches what's in the table; and whether the tool has enough reach "+
				"(flute length, stickout length, and the pocket's own length offset) for the depth "+
				"the program cuts to. Returns a one-paragraph summary you can act on directly."),
		mcpsdk.WithInputSchema[CheckToolsArgs](),
		mcpsdk.WithReadOnlyHintAnnotation(true),
	)
	s.AddTool(tool, checkToolsHandler(d))
}

func checkToolsHandler(d Deps) server.ToolHandlerFunc {
	return func(_ context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		var args CheckToolsArgs
		if err := req.BindArguments(&args); err != nil {
			return mcpsdk.NewToolResultError(err.Error()), nil
		}

		hasSidecar := args.Sidecar != nil
		hasTools := len(args.Tools) > 0
		switch {
		case !hasSidecar && !hasTools:
			return mcpsdk.NewToolResultError("check_tools: provide either sidecar or tools"), nil
		case hasSidecar && hasTools:
			return mcpsdk.NewToolResultError("check_tools: provide either sidecar or tools, not both"), nil
		}

		sidecar := args.Sidecar
		if sidecar == nil {
			sidecar = &cnc.Sidecar{Tools: make([]cnc.SidecarTool, 0, len(args.Tools))}
			for _, t := range args.Tools {
				if t.T <= 0 {
					return mcpsdk.NewToolResultError(
						fmt.Sprintf("check_tools: tool entry has invalid t=%d, must be >= 1", t.T)), nil
				}
				sidecar.Tools = append(sidecar.Tools, cnc.SidecarTool{
					TNumber:        t.T,
					Diameter:       t.Diameter,
					FluteLength:    t.FluteLength,
					StickoutLength: t.StickoutLength,
					MaxDepth:       t.MaxDepth,
				})
			}
		}

		resolvedID := args.MachineID
		if d.Registry != nil {
			if _, id := d.Registry.Streamer(args.MachineID); id != "" {
				resolvedID = id
			}
		}

		var table *cnc.ToolTable
		if d.LatestToolTable != nil {
			if t, err := d.LatestToolTable(resolvedID); err == nil {
				table = t
			}
		}

		cfg := cnc.ReconcileConfig{ClearanceMargin: args.Clearance}
		report := cnc.ReconcileTools(sidecar, table, cfg)

		result := CheckToolsResult{
			MachineID:  resolvedID,
			TableFound: table != nil,
			Report:     report,
			Summary:    Summarize(report),
		}
		if table != nil {
			result.TableReadAt = table.ReadAt.UTC().Format("2006-01-02 15:04:05Z")
		}
		return mcpsdk.NewToolResultStructured(result, result.Summary), nil
	}
}

// Summarize renders a cnc.ReconcileTools report as a one-paragraph,
// plain-English readout: one short sentence per tool that needs
// attention (not loaded, moved, diameter mismatch, or not enough
// reach), or a single all-clear sentence when every tool matches.
func Summarize(report *cnc.ToolReconcileReport) string {
	if report == nil || len(report.Tools) == 0 {
		return "No tools to check."
	}

	var sentences []string
	for _, t := range report.Tools {
		switch t.PocketStatus {
		case cnc.PocketMissing:
			sentences = append(sentences, fmt.Sprintf(
				"T%d not loaded: load into pocket %d and touch off.", t.TNumber, t.TNumber))
		case cnc.PocketMoved:
			actual := 0
			if t.ActualPocket != nil {
				actual = *t.ActualPocket
			}
			sentences = append(sentences, fmt.Sprintf(
				"T%d found in pocket %d instead of %d: confirm the offset before running or swap it back.",
				t.TNumber, actual, t.TNumber))
		}
		if t.LengthInsufficient {
			sentences = append(sentences, fmt.Sprintf(
				"T%d needs %s reach, %s: choose a longer tool or reduce depth.",
				t.TNumber, formatNum(t.RequiredReach), lengthShortfallDetail(t)))
		}
		if t.DiameterWarning && t.ActualDiameter != nil {
			sentences = append(sentences, fmt.Sprintf(
				"T%d diameter mismatch: expected %s, table reports %s.",
				t.TNumber, formatNum(t.ExpectedDiameter), formatNum(*t.ActualDiameter)))
		}
	}

	if len(sentences) == 0 {
		return fmt.Sprintf("All %d tool(s) match the tool table — ready to run.", len(report.Tools))
	}
	return strings.Join(sentences, " ")
}

// lengthShortfallDetail names whichever length figure the tool falls
// short against — flute, stickout, or the pocket's own machine length
// offset — matching the ordering ReconcileTools itself checks in.
func lengthShortfallDetail(t cnc.ToolReconcileResult) string {
	switch {
	case t.FluteLength > 0 && t.RequiredReach > t.FluteLength:
		return "flute is " + formatNum(t.FluteLength)
	case t.StickoutLength > 0 && t.RequiredReach > t.StickoutLength:
		return "stickout is " + formatNum(t.StickoutLength)
	case t.MachineLengthOffset != nil && *t.MachineLengthOffset < t.RequiredReach:
		return "pocket length offset is " + formatNum(*t.MachineLengthOffset)
	default:
		return t.LengthReason
	}
}

// formatNum renders a float with up to 4 decimal places, trimming
// trailing zeros (and a trailing bare decimal point) so "27.5000"
// reads as "27.5" and "26.0000" reads as "26".
func formatNum(v float64) string {
	s := strconv.FormatFloat(v, 'f', 4, 64)
	s = strings.TrimRight(s, "0")
	s = strings.TrimSuffix(s, ".")
	return s
}
