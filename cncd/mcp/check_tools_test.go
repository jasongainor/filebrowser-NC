package mcp

import (
	"testing"
	"time"

	"github.com/filebrowser/filebrowser/v2/cnc"
)

func TestCheckTools_SimplifiedList_HappyPath(t *testing.T) {
	table := &cnc.ToolTable{
		MachineID: "m1",
		ReadAt:    time.Now(),
		Slots: []cnc.ToolTableSlot{
			{Slot: 5, LengthGeom: ptr(2.0), DiameterGeom: ptr(0.25)},
		},
	}
	d := newTestDeps(t, table)

	res := callTool(t, checkToolsHandler(d), map[string]any{
		"tools": []map[string]any{
			{"t": 5, "diameter": 0.25, "flute_length": 0.75, "max_depth": 0.5},
		},
	})
	if res.IsError {
		t.Fatalf("unexpected tool error: %+v", res)
	}
	result := structuredOf(t, res).(CheckToolsResult)
	if result.Report.Summary.Match != 1 {
		t.Fatalf("Summary = %+v, want 1 match", result.Report.Summary)
	}
}

func TestCheckTools_FullSidecar_HappyPath(t *testing.T) {
	table := &cnc.ToolTable{
		MachineID: "m1",
		ReadAt:    time.Now(),
		Slots: []cnc.ToolTableSlot{
			{Slot: 5, LengthGeom: ptr(2.0), DiameterGeom: ptr(0.25)},
		},
	}
	d := newTestDeps(t, table)

	res := callTool(t, checkToolsHandler(d), map[string]any{
		"sidecar": map[string]any{
			"schema_version": 1,
			"job":            "J000020",
			"tools": []map[string]any{
				{"t_number": 5, "diameter": 0.25},
			},
		},
	})
	if res.IsError {
		t.Fatalf("unexpected tool error: %+v", res)
	}
	result := structuredOf(t, res).(CheckToolsResult)
	if result.Report.Summary.Match != 1 {
		t.Fatalf("Summary = %+v, want 1 match", result.Report.Summary)
	}
}

func TestCheckTools_ValidationErrors(t *testing.T) {
	d := newTestDeps(t, nil)

	t.Run("neither sidecar nor tools", func(t *testing.T) {
		res := callTool(t, checkToolsHandler(d), map[string]any{})
		if !res.IsError {
			t.Fatalf("expected an error result, got %+v", res)
		}
	})

	t.Run("both sidecar and tools", func(t *testing.T) {
		res := callTool(t, checkToolsHandler(d), map[string]any{
			"sidecar": map[string]any{"schema_version": 1, "job": "J1", "tools": []map[string]any{}},
			"tools":   []map[string]any{{"t": 1, "diameter": 0.25}},
		})
		if !res.IsError {
			t.Fatalf("expected an error result, got %+v", res)
		}
	})

	t.Run("invalid tool number", func(t *testing.T) {
		res := callTool(t, checkToolsHandler(d), map[string]any{
			"tools": []map[string]any{{"t": 0, "diameter": 0.25}},
		})
		if !res.IsError {
			t.Fatalf("expected an error result, got %+v", res)
		}
	})
}

// TestSummarize_Golden pins the exact plain-English wording for the
// three cases check_tools is asked to explain: a tool moved to
// another pocket, a tool missing entirely, and a tool that doesn't
// have enough reach for its own program. Table/sidecar are built by
// hand (not through a live read) so the golden string is stable.
func TestSummarize_Golden(t *testing.T) {
	table := &cnc.ToolTable{
		Slots: []cnc.ToolTableSlot{
			// T5 expected in pocket 5, actually sitting in pocket 8.
			{Slot: 8, LengthGeom: ptr(3.0), DiameterGeom: ptr(0.25)},
			// T1 loaded where expected, but too short for its own program.
			{Slot: 1, LengthGeom: ptr(1.2), DiameterGeom: ptr(0.5)},
			// T12 nowhere in the table at all.
		},
	}
	sidecar := &cnc.Sidecar{
		SchemaVersion: 1,
		Job:           "J000020",
		Tools: []cnc.SidecarTool{
			{TNumber: 5, Diameter: 0.25, FluteLength: 4.0, StickoutLength: 4.0, MaxDepth: 1.0},
			{TNumber: 1, Diameter: 0.5, FluteLength: 1.04, StickoutLength: 2.0, MaxDepth: 1.0},
			{TNumber: 12, Diameter: 0.375, FluteLength: 2.0, StickoutLength: 2.0, MaxDepth: 0.5},
		},
	}

	report := cnc.ReconcileTools(sidecar, table, cnc.ReconcileConfig{ClearanceMargin: 0.1})
	got := Summarize(report)

	want := "T5 found in pocket 8 instead of 5: confirm the offset before running or swap it back. " +
		"T1 needs 1.1 reach, flute is 1.04: choose a longer tool or reduce depth. " +
		"T12 not loaded: load into pocket 12 and touch off."
	if got != want {
		t.Fatalf("Summarize() =\n%q\nwant\n%q", got, want)
	}
}

func TestSummarize_AllMatch(t *testing.T) {
	table := &cnc.ToolTable{
		Slots: []cnc.ToolTableSlot{
			{Slot: 1, LengthGeom: ptr(4.0), DiameterGeom: ptr(0.25)},
		},
	}
	sidecar := &cnc.Sidecar{
		Tools: []cnc.SidecarTool{
			{TNumber: 1, Diameter: 0.25, FluteLength: 3.0, StickoutLength: 3.0, MaxDepth: 0.5},
		},
	}
	report := cnc.ReconcileTools(sidecar, table, cnc.ReconcileConfig{})
	got := Summarize(report)
	want := "All 1 tool(s) match the tool table — ready to run."
	if got != want {
		t.Fatalf("Summarize() = %q, want %q", got, want)
	}
}
