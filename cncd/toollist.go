package cncd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/filebrowser/filebrowser/v2/cnc"
	"github.com/filebrowser/filebrowser/v2/cncapi"
	"github.com/filebrowser/filebrowser/v2/settings"
)

// errMachineNotFound is returned by buildMachineToolList when the
// requested machine ID (or the default, if none configured) doesn't
// match anything in the current config.
var errMachineNotFound = fmt.Errorf("machine not found")

// toolTableShareDir mirrors http/cnc.go's constant of the same name:
// the root-relative folder tool-table JSON dumps live under.
const toolTableShareDir = "cnc-tool-tables"

// sanitizeMachineID mirrors http/cnc.go's sanitizeMachineID.
func sanitizeMachineID(id string) string {
	if id == "" {
		return "default"
	}
	repl := strings.NewReplacer(
		"/", "_",
		"\\", "_",
		"..", "_",
		string(os.PathSeparator), "_",
	)
	cleaned := repl.Replace(id)
	if cleaned == "" {
		return "default"
	}
	return cleaned
}

// toolTableDirAbs resolves the absolute tool-table dump directory for
// a machine, root-jailed. Mirrors http/cnc.go's toolTableDirAbs, but
// through the seam's PathResolver instead of a filebrowser user.
func toolTableDirAbs(resolver cncapi.PathResolver, machineID string) (string, error) {
	rel := filepath.Join(toolTableShareDir, sanitizeMachineID(machineID))
	return resolver.FullPath(rel)
}

// newestJSONIn mirrors http/cnc.go's newestJSONIn: the most
// recently-modified *.json file directly inside dir, or "" (no error)
// when dir doesn't exist yet.
func newestJSONIn(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	var (
		newest    string
		newestMod time.Time
	)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(newestMod) {
			newestMod = info.ModTime()
			newest = filepath.Join(dir, e.Name())
		}
	}
	return newest, nil
}

// effectiveUnits mirrors http/cnc_toollist.go's effectiveUnits.
func effectiveUnits(_ *settings.Machine) string {
	if u := os.Getenv("CNC_UNITS"); u == "mm" || u == "in" {
		return u
	}
	return "in"
}

// findMachine mirrors http/cnc.go's findMachine: the Machine matching
// id, or Machines[0] when id is empty. nil if none configured.
func findMachine(c settings.Cnc, id string) *settings.Machine {
	if len(c.Machines) == 0 {
		return nil
	}
	if id == "" {
		return &c.Machines[0]
	}
	for i := range c.Machines {
		if c.Machines[i].ID == id {
			return &c.Machines[i]
		}
	}
	return nil
}

// buildMachineToolList mirrors http/cnc_toollist.go's function of the
// same name: the engine shared by the per-machine toollist view and
// the /api/displays/{id} firmware endpoint. Returns nil with an error
// when the machine is unknown.
func buildMachineToolList(registry *cnc.Registry, cfg settings.Cnc, root string, machineID string) (*cnc.ToolList, error) {
	m := findMachine(cfg, machineID)
	if m == nil {
		return nil, errMachineNotFound
	}

	connected := false
	if st, _ := registry.Streamer(m.ID); st != nil {
		connected = st.Alive()
	}

	resolver := cncapi.NewRootResolver(root)
	var tbl *cnc.ToolTable
	dir, err := toolTableDirAbs(resolver, m.ID)
	if err != nil {
		return nil, err
	}
	if latestPath, _ := newestJSONIn(dir); latestPath != "" {
		if buf, rerr := os.ReadFile(latestPath); rerr == nil {
			var t cnc.ToolTable
			if json.Unmarshal(buf, &t) == nil {
				tbl = &t
			}
		}
	}

	var lib *cnc.ToolLibrary
	if store := registry.LibraryStore(); store != nil {
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
