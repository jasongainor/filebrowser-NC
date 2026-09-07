package mcp

import (
	"testing"
	"time"

	"github.com/filebrowser/filebrowser/v2/cnc"
)

func TestToolTable_HappyPath_PersistedDump(t *testing.T) {
	readAt := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	table := &cnc.ToolTable{
		MachineID: "m1",
		ReadAt:    readAt,
		Slots: []cnc.ToolTableSlot{
			{Slot: 1, LengthGeom: ptr(4.5), DiameterGeom: ptr(0.25)},
			{Slot: 2, Empty: true},
		},
	}
	d := newTestDeps(t, table)

	res := callTool(t, toolTableHandler(d), map[string]any{})
	if res.IsError {
		t.Fatalf("unexpected tool error: %+v", res)
	}
	result, ok := structuredOf(t, res).(ToolTableResult)
	if !ok {
		t.Fatalf("structured content is %T, want ToolTableResult", res.StructuredContent)
	}
	if result.Refreshed {
		t.Errorf("Refreshed = true without refresh requested")
	}
	if result.Table == nil || !result.Table.ReadAt.Equal(readAt) {
		t.Errorf("Table = %+v, want the persisted dump", result.Table)
	}
}

func TestToolTable_NoDumpYet(t *testing.T) {
	d := newTestDeps(t, nil)
	res := callTool(t, toolTableHandler(d), map[string]any{})
	if res.IsError {
		t.Fatalf("unexpected tool error: %+v", res)
	}
	result := structuredOf(t, res).(ToolTableResult)
	if result.Table != nil {
		t.Errorf("Table = %+v, want nil (no dump ever read)", result.Table)
	}
}

func TestToolTable_RefreshSkipped_NotConnected(t *testing.T) {
	table := &cnc.ToolTable{MachineID: "m1", ReadAt: time.Now()}
	d := newTestDeps(t, table)

	res := callTool(t, toolTableHandler(d), map[string]any{"refresh": true})
	if res.IsError {
		t.Fatalf("unexpected tool error: %+v", res)
	}
	result := structuredOf(t, res).(ToolTableResult)
	if result.Refreshed {
		t.Errorf("Refreshed = true, want false — the test machine has no live link")
	}
	if result.RefreshNote == "" {
		t.Errorf("expected a RefreshNote explaining the skip")
	}
	// The persisted dump should still come back even though refresh
	// was skipped — a caller shouldn't lose data because they asked
	// for fresher data that wasn't available.
	if result.Table == nil {
		t.Errorf("Table = nil, want the persisted dump to survive a skipped refresh")
	}
}

func TestToolTable_UnknownMachine(t *testing.T) {
	d := newTestDeps(t, nil)
	res := callTool(t, toolTableHandler(d), map[string]any{"machine_id": "nope"})
	if !res.IsError {
		t.Fatalf("expected an error result for an unknown machine, got %+v", res)
	}
}

func ptr(f float64) *float64 { return &f }
