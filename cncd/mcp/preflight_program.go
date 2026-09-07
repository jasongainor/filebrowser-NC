package mcp

import (
	"context"
	"fmt"
	"path"
	"strconv"
	"strings"

	mcpsdk "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/filebrowser/filebrowser/v2/cnc"
)

// PreflightProgramArgs is preflight_program's input.
type PreflightProgramArgs struct {
	FilePath  string `json:"file_path" jsonschema_description:"NC file path, relative to the daemon's served root (the share)."`
	MachineID string `json:"machine_id,omitempty" jsonschema_description:"Machine ID to preflight against. Omit to use the daemon's default (first configured) machine."`
}

// PreflightProgramResult wraps cnc.BuildPreflight's result with a
// short plain-text summary.
type PreflightProgramResult struct {
	Preflight *cnc.Preflight `json:"preflight"`
	Summary   string         `json:"summary"`
}

func registerPreflightProgram(s *server.MCPServer, d Deps) {
	tool := mcpsdk.NewTool("preflight_program",
		mcpsdk.WithDescription(
			"Parse an NC file's tool references and compare them against the machine's latest "+
				"tool-table read: per-tool ok/warn/empty/offline/missing status, the starting tool, "+
				"a swap-on-send warning if the controller's current spindle tool differs from the "+
				"program's first tool, and — when the file carries a GMW identity header — the "+
				"sidecar-based tool reconciliation (see the program_identity resource) as an "+
				"additional 'identity' section."),
		mcpsdk.WithInputSchema[PreflightProgramArgs](),
		mcpsdk.WithReadOnlyHintAnnotation(true),
	)
	s.AddTool(tool, preflightProgramHandler(d))
}

func preflightProgramHandler(d Deps) server.ToolHandlerFunc {
	return func(_ context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		var args PreflightProgramArgs
		if err := req.BindArguments(&args); err != nil {
			return mcpsdk.NewToolResultError(err.Error()), nil
		}
		if strings.TrimSpace(args.FilePath) == "" {
			return mcpsdk.NewToolResultError("preflight_program: file_path is required"), nil
		}
		if d.Resolver == nil {
			return mcpsdk.NewToolResultError("cncd: no path resolver configured"), nil
		}

		clean := path.Clean(ensureLeading(args.FilePath))
		if strings.Contains(clean, "..") {
			return mcpsdk.NewToolResultError("preflight_program: file_path must not escape the share"), nil
		}
		absPath, err := d.Resolver.FullPath(clean)
		if err != nil {
			return mcpsdk.NewToolResultError(err.Error()), nil
		}

		resolvedID := args.MachineID
		var ag *cnc.Aggregator
		if d.Registry != nil {
			if _, id := d.Registry.Streamer(args.MachineID); id != "" {
				resolvedID = id
			}
			ag, _ = d.Registry.Aggregator(args.MachineID)
		}

		var table *cnc.ToolTable
		if d.LatestToolTable != nil {
			if t, terr := d.LatestToolTable(resolvedID); terr == nil {
				table = t
			}
		}

		spindleTool := currentSpindleTool(ag)

		pf, err := cnc.BuildPreflight(absPath, clean, resolvedID, table, spindleTool)
		if err != nil {
			return mcpsdk.NewToolResultError(err.Error()), nil
		}

		result := PreflightProgramResult{
			Preflight: pf,
			Summary:   preflightSummary(pf),
		}
		return mcpsdk.NewToolResultStructured(result, result.Summary), nil
	}
}

func preflightSummary(pf *cnc.Preflight) string {
	if pf == nil {
		return "No preflight result."
	}
	if pf.TableMissing {
		return fmt.Sprintf("%s: no tool-table read on file for %s — read the table before sending.",
			pf.FilePath, pf.MachineID)
	}
	msg := fmt.Sprintf("%s: %d ok, %d warn, %d empty, %d offline, %d missing.",
		pf.FilePath, pf.Summary.OK, pf.Summary.Warn, pf.Summary.Empty, pf.Summary.Offline, pf.Summary.Missing)
	if pf.SpindleSwap {
		msg += " Controller's current tool differs from the program's starting tool — expect an M06 swap on send."
	}
	if pf.Identity != nil {
		msg += fmt.Sprintf(" Identity: job=%s part=%s sha=%s.", pf.Identity.Job, pf.Identity.Part, pf.Identity.SHAStatus)
	}
	return msg
}

// ensureLeading mirrors http/cnc.go's ensureLeading: a bare relative
// path is treated as rooted, since every NC path here is relative to
// the served share, not the process's working directory.
func ensureLeading(p string) string {
	if strings.HasPrefix(p, "/") {
		return p
	}
	return "/" + p
}

// currentSpindleTool reads the aggregator's "tool" metric (Q201) and
// parses it to an int, mirroring http/cnc.go's unexported helper of
// the same name (that package can't be imported from here — it pulls
// in the whole filebrowser HTTP stack — so the ~15 lines of parsing
// are duplicated rather than shared). Returns nil when ag is nil, the
// metric is missing, stale, or unparseable — callers treat nil as
// "swap unknown".
func currentSpindleTool(ag *cnc.Aggregator) *int {
	if ag == nil {
		return nil
	}
	snap := ag.Snapshot()
	m, ok := snap["tool"]
	if !ok || m == nil || m.Stale {
		return nil
	}
	switch v := m.Parsed.(type) {
	case int:
		x := v
		return &x
	case int64:
		x := int(v)
		return &x
	case float64:
		x := int(v)
		return &x
	}
	if m.Value != "" {
		s := strings.TrimSpace(m.Value)
		// Q201 frame shape: "STATUS,TOOL,5". Take the trailing token.
		if i := strings.LastIndex(s, ","); i >= 0 {
			s = strings.TrimSpace(s[i+1:])
		}
		if n, err := strconv.Atoi(s); err == nil {
			return &n
		}
	}
	return nil
}
