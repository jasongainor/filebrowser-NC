package mcp

import (
	"testing"

	"github.com/filebrowser/filebrowser/v2/cnc"
	"github.com/filebrowser/filebrowser/v2/cncapi"
	"github.com/filebrowser/filebrowser/v2/settings"
)

// fakeSettings satisfies the unexported settingsReader interface
// cnc.NewRegistry expects (Get() (*settings.Settings, error)) without
// needing a real Store — cncd/mcp can't import cncd's Store (cncd
// imports this package, not the other way around), and doesn't need
// to: a registry configured with an empty Host never dials anywhere
// real (cnc.Streamer.resolveMachine returns ErrConfigMissing first),
// exactly like cncd's own newTestDeps (cncd/router_test.go).
type fakeSettings struct {
	cnc settings.Cnc
}

func (f *fakeSettings) Get() (*settings.Settings, error) {
	return &settings.Settings{Cnc: f.cnc}, nil
}

func findTestMachine(cfg settings.Cnc, id string) *settings.Machine {
	if len(cfg.Machines) == 0 {
		return nil
	}
	if id == "" {
		return &cfg.Machines[0]
	}
	for i := range cfg.Machines {
		if cfg.Machines[i].ID == id {
			return &cfg.Machines[i]
		}
	}
	return nil
}

// newTestDeps builds a Deps with one configured machine ("m1", empty
// Host so the registry's background Link never dials anywhere) and a
// fresh temp directory as the served root. table is returned by
// LatestToolTable for every machine ID; nil is a legitimate "no dump
// yet" value.
func newTestDeps(t *testing.T, table *cnc.ToolTable) Deps {
	t.Helper()
	root := t.TempDir()
	fs := &fakeSettings{cnc: settings.Cnc{
		Machines: []settings.Machine{{ID: "m1", Name: "Machine 1"}},
	}}
	registry := cnc.NewRegistry(fs)
	t.Cleanup(registry.Stop)

	return Deps{
		Registry: registry,
		Resolver: cncapi.NewRootResolver(root),
		Root:     root,
		FindMachine: func(id string) *settings.Machine {
			return findTestMachine(fs.cnc, id)
		},
		LatestToolTable: func(string) (*cnc.ToolTable, error) {
			return table, nil
		},
	}
}

func TestNewServer_RegistersAllTools(t *testing.T) {
	d := newTestDeps(t, nil)
	s := NewServer(d)

	want := []string{"machine_state", "tool_table", "check_tools", "preflight_program", "list_programs"}
	got := s.ListTools()
	if len(got) != len(want) {
		t.Fatalf("got %d tools, want %d (%v)", len(got), len(want), got)
	}
	for _, name := range want {
		tool, ok := got[name]
		if !ok {
			t.Errorf("missing tool %q", name)
			continue
		}
		if tool.Tool.Description == "" {
			t.Errorf("tool %q has no description", name)
		}
	}
}

func TestNewServer_RegistersProgramIdentityResource(t *testing.T) {
	d := newTestDeps(t, nil)
	s := NewServer(d)

	resources := s.ListResources()
	entry, ok := resources[programIdentityResourceURI]
	if !ok {
		t.Fatalf("missing resource %q; got %v", programIdentityResourceURI, resources)
	}
	if entry.Resource.MIMEType != "text/markdown" {
		t.Errorf("resource MIME type = %q, want text/markdown", entry.Resource.MIMEType)
	}
}
