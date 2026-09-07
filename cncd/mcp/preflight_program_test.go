package mcp

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/filebrowser/filebrowser/v2/cnc"
)

const testNC = "%\nO0001\n(T5 CARBIDE EM)\nT5 M06\nG54 G0 X0 Y0\nM30\n%\n"

func writeTestNC(t *testing.T, root, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func TestPreflightProgram_HappyPath(t *testing.T) {
	table := &cnc.ToolTable{
		MachineID: "m1",
		ReadAt:    time.Now(),
		Slots: []cnc.ToolTableSlot{
			{Slot: 5, LengthGeom: ptr(2.0), DiameterGeom: ptr(0.25)},
		},
	}
	d := newTestDeps(t, table)
	writeTestNC(t, d.Root, "part.nc", testNC)

	res := callTool(t, preflightProgramHandler(d), map[string]any{"file_path": "part.nc"})
	if res.IsError {
		t.Fatalf("unexpected tool error: %+v", res)
	}
	result := structuredOf(t, res).(PreflightProgramResult)
	if result.Preflight.Summary.OK != 1 {
		t.Fatalf("Summary = %+v, want 1 ok", result.Preflight.Summary)
	}
}

func TestPreflightProgram_MissingFilePath(t *testing.T) {
	d := newTestDeps(t, nil)
	res := callTool(t, preflightProgramHandler(d), map[string]any{})
	if !res.IsError {
		t.Fatalf("expected an error result, got %+v", res)
	}
}

func TestPreflightProgram_EscapesShare(t *testing.T) {
	d := newTestDeps(t, nil)
	res := callTool(t, preflightProgramHandler(d), map[string]any{"file_path": "../../etc/passwd"})
	if !res.IsError {
		t.Fatalf("expected an error result for a path escape, got %+v", res)
	}
}

func TestPreflightProgram_FileNotFound(t *testing.T) {
	d := newTestDeps(t, nil)
	res := callTool(t, preflightProgramHandler(d), map[string]any{"file_path": "nope.nc"})
	if !res.IsError {
		t.Fatalf("expected an error result for a missing file, got %+v", res)
	}
}
