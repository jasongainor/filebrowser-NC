package mcp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/filebrowser/filebrowser/v2/cnc"
)

const identityNC = "(GMW-ID V1)\n(GMW-JOB J000020 OP10)\n(GMW-PART F-BRACKET-REV-C)\n" +
	"(GMW-POST HAAS-NGC V1.0.3)\n(GMW-POSTED 2026-09-07T14:22:00Z)\n(GMW-TOOLS 1)\n" +
	"(GMW-SHA PENDING)\nO0001\nT5 M06\nM30\n"

func TestListPrograms_HappyPath(t *testing.T) {
	d := newTestDeps(t, nil)
	writeTestNC(t, d.Root, "plain.nc", testNC)
	writeTestNC(t, d.Root, "identified.nc", identityNC)
	// A sidecar next to identified.nc should not show up as its own
	// program entry.
	writeTestNC(t, d.Root, "identified.nc.gmw.json", `{"schema_version":1,"job":"J000020","tools":[]}`)
	if err := os.MkdirAll(filepath.Join(d.Root, "jobs"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeTestNC(t, d.Root, "jobs/nested.nc", testNC)
	// Tool-table dumps live under this dir — must be skipped entirely.
	if err := os.MkdirAll(filepath.Join(d.Root, "cnc-tool-tables", "m1"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeTestNC(t, d.Root, "cnc-tool-tables/m1/dump.json", `{}`)

	res := callTool(t, listProgramsHandler(d), map[string]any{})
	if res.IsError {
		t.Fatalf("unexpected tool error: %+v", res)
	}
	result := structuredOf(t, res).(ListProgramsResult)

	if len(result.Programs) != 3 {
		t.Fatalf("got %d programs, want 3 (plain.nc, identified.nc, jobs/nested.nc): %+v", len(result.Programs), result.Programs)
	}

	byPath := map[string]ProgramEntry{}
	for _, p := range result.Programs {
		byPath[p.Path] = p
	}
	if _, ok := byPath["cnc-tool-tables/m1/dump.json"]; ok {
		t.Errorf("tool-table dump directory should have been skipped")
	}
	if _, ok := byPath["identified.nc.gmw.json"]; ok {
		t.Errorf("sidecar file should not be listed as its own program")
	}

	plain, ok := byPath["plain.nc"]
	if !ok || plain.HasIdentity {
		t.Errorf("plain.nc = %+v, want present with HasIdentity=false", plain)
	}

	identified, ok := byPath["identified.nc"]
	if !ok || !identified.HasIdentity {
		t.Fatalf("identified.nc = %+v, want present with HasIdentity=true", identified)
	}
	if identified.Job != "J000020" || identified.Operation != "OP10" || identified.Part != "F-BRACKET-REV-C" {
		t.Errorf("identified.nc badge = %+v, want job=J000020 op=OP10 part=F-BRACKET-REV-C", identified)
	}
	if identified.SHAStatus != cnc.SHAUnstamped {
		t.Errorf("SHAStatus = %q, want %q (GMW-SHA PENDING)", identified.SHAStatus, cnc.SHAUnstamped)
	}

	nested, ok := byPath["jobs/nested.nc"]
	if !ok {
		t.Errorf("expected jobs/nested.nc to be listed recursively")
	}
	if nested.JobFolder != "jobs" {
		t.Errorf("jobs/nested.nc JobFolder = %q, want %q", nested.JobFolder, "jobs")
	}
	if plain.JobFolder != "" {
		t.Errorf("plain.nc (loose at root) JobFolder = %q, want empty", plain.JobFolder)
	}
}

func TestListPrograms_UnknownPath(t *testing.T) {
	d := newTestDeps(t, nil)
	res := callTool(t, listProgramsHandler(d), map[string]any{"path": "/does-not-exist"})
	if !res.IsError {
		t.Fatalf("expected an error result for a nonexistent path, got %+v", res)
	}
}

func TestListPrograms_FilterByJob(t *testing.T) {
	d := newTestDeps(t, nil)
	if err := os.MkdirAll(filepath.Join(d.Root, "J000020 - F-BRACKET-REV-C"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeTestNC(t, d.Root, "J000020 - F-BRACKET-REV-C/op10.nc", identityNC)
	writeTestNC(t, d.Root, "unrelated.nc", testNC)
	// A file sitting in a job folder but with no header of its own —
	// still matches the filter by folder name, not by header.
	writeTestNC(t, d.Root, "J000020 - F-BRACKET-REV-C/readme.nc", testNC)

	// Filter by folder name.
	res := callTool(t, listProgramsHandler(d), map[string]any{"job": "J000020 - F-BRACKET-REV-C"})
	if res.IsError {
		t.Fatalf("unexpected tool error: %+v", res)
	}
	result := structuredOf(t, res).(ListProgramsResult)
	if len(result.Programs) != 2 {
		t.Fatalf("filter by folder name: got %d programs, want 2: %+v", len(result.Programs), result.Programs)
	}

	// Filter by the parsed GMW-JOB id, case-insensitively.
	res = callTool(t, listProgramsHandler(d), map[string]any{"job": "j000020"})
	if res.IsError {
		t.Fatalf("unexpected tool error: %+v", res)
	}
	result = structuredOf(t, res).(ListProgramsResult)
	if len(result.Programs) != 1 || result.Programs[0].Path != "J000020 - F-BRACKET-REV-C/op10.nc" {
		t.Fatalf("filter by job id: got %+v, want just op10.nc (the identified one)", result.Programs)
	}

	// A filter that matches nothing returns an empty (not nil-error) list.
	res = callTool(t, listProgramsHandler(d), map[string]any{"job": "NOPE"})
	if res.IsError {
		t.Fatalf("unexpected tool error: %+v", res)
	}
	result = structuredOf(t, res).(ListProgramsResult)
	if len(result.Programs) != 0 {
		t.Fatalf("filter with no match: got %+v, want empty", result.Programs)
	}
}
