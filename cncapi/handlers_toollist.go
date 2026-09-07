package cncapi

// Shared logic behind GET /api/machines/{id}/toollist and the
// tool-list half of GET /api/displays/{id}. Mirrors
// http/cnc_toollist.go's cncMachineToolListHandler /
// buildMachineToolList, and replaces cncd/toollist.go's
// pre-extraction independent copy of the same function.

import (
	"errors"
	"os"

	"github.com/filebrowser/filebrowser/v2/cnc"
	"github.com/filebrowser/filebrowser/v2/settings"
)

// ErrMachineNotFound is returned by BuildMachineToolList when the
// requested machine ID (or the default, if machineID is empty)
// doesn't match anything in the current config. Callers map it to a
// 404; any other error from BuildMachineToolList is a 500 (a
// tool-table dump directory that fails to resolve, which in practice
// never happens against a well-formed PathResolver).
var ErrMachineNotFound = errors.New("machine not found")

// BuildMachineToolList is the engine shared by the per-machine
// /toollist view AND the /displays/{id} firmware endpoint. Returns
// ErrMachineNotFound (status 404) when machineID doesn't resolve.
func (d Deps) BuildMachineToolList(machineID string) (*cnc.ToolList, error) {
	cfg := d.Store.Snapshot()
	m := FindMachine(cfg, machineID)
	if m == nil {
		return nil, ErrMachineNotFound
	}

	// Connected = the controller actually answered recently (the
	// always-on baseline poller keeps Streamer.Alive() fresh even
	// with no operator watching). See http/cnc_toollist.go's longer
	// comment on why this isn't ag.IsAwake().
	connected := false
	if st, _ := d.Registry.Streamer(m.ID); st != nil {
		connected = st.Alive()
	}

	tbl, err := d.LatestToolTable(m.ID)
	if err != nil {
		return nil, err
	}

	var lib *cnc.ToolLibrary
	if store := d.Registry.LibraryStore(); store != nil {
		lib = store.Library()
	}

	units := effectiveUnits(m)
	pocketCount := m.EffectiveToolSlots()
	return cnc.BuildToolList(
		m.ID,
		m.Name,
		units,
		connected,
		pocketCount,
		200, // Haas NGC table max
		tbl,
		lib,
	), nil
}

// effectiveUnits returns "in" or "mm". Mirrors
// http/cnc_toollist.go's effectiveUnits: no per-machine "units" field
// exists yet, so this honors an explicit env override (CNC_UNITS=mm)
// and otherwise defaults to inches.
func effectiveUnits(_ *settings.Machine) string {
	if u := os.Getenv("CNC_UNITS"); u == "mm" || u == "in" {
		return u
	}
	return "in"
}
