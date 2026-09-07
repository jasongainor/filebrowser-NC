// Package cncapi holds the seam between the CNC handlers (cnc/,
// http/cnc*.go) and whatever hosts them, plus (in this file and its
// siblings, handlers_*.go) the actual handler bodies shared by both
// hosts. See seam.go's package doc for the PathResolver/Authz half of
// the story; this file adds the third and fourth capabilities a
// shared handler needs — a live *cnc.Registry and a SettingsStore —
// bundled into a Deps value, plus the small helpers every handler
// family in this package builds on (path validation, tool-table dump
// directory resolution, machine lookup).
//
// Every exported function here mirrors one that used to live only in
// http/cnc*.go (filebrowser's fbhttp package) — see each function's
// doc comment for which one. filebrowser now calls through to these
// via thin wrappers so its behavior is unchanged; cncd
// (cncd/router_cnc.go) is the second caller, wiring the same bodies
// up to a bearer-token Authz instead of a session.
package cncapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/filebrowser/filebrowser/v2/cnc"
	"github.com/filebrowser/filebrowser/v2/settings"
)

// errToStatusDefault is a small, filebrowser-independent analog of
// http/utils.go's errToStatus for the generic (os.*, plain
// fmt.Errorf) errors cnc/*.go's own APIs (QueueStore, LibraryStore,
// etc.) return. It doesn't know about filebrowser's own error
// sentinels (errors.ErrNotExist, etc.) — those are only ever returned
// by filebrowser's own code, never by cnc/, so callers that need that
// mapping (the http/cnc*.go wrappers) keep using errToStatus directly
// on errors that came from d.pathResolver()/d.store.
func errToStatusDefault(err error) int {
	switch {
	case err == nil:
		return 200
	case os.IsNotExist(err):
		return 404
	case os.IsExist(err):
		return 409
	case os.IsPermission(err):
		return 403
	case errors.Is(err, os.ErrDeadlineExceeded):
		return 504
	default:
		return 500
	}
}

// SettingsStore is the seam through which shared handlers read and
// mutate the settings.Cnc document. Both hosts' persistence already
// fits this exact shape: cncd's *cncd.Store has Snapshot() and
// Update(func(*settings.Cnc) error) today (cncd/config.go), and
// filebrowser's adapter (http/cnc_seam.go) wraps its *data the same
// way — Update mutates d.settings.Cnc in place and saves the whole
// settings document through d.store.Settings.Save.
type SettingsStore interface {
	// Snapshot returns a copy of the current settings.Cnc document.
	Snapshot() settings.Cnc
	// Update applies fn to the live settings.Cnc under whatever
	// locking/persistence the host needs, and persists the result.
	// fn returning an error aborts the update — nothing is persisted.
	Update(fn func(*settings.Cnc) error) error
}

// Deps bundles what a shared CNC handler needs from its host: the
// live registry, the two seam capabilities from seam.go, and a
// SettingsStore. Handlers in this package take a Deps value (plus
// whatever's specific to that one request) instead of reaching into a
// filebrowser *data or a cncd Deps directly.
type Deps struct {
	Registry *cnc.Registry
	Resolver PathResolver
	Authz    Authz
	Store    SettingsStore
}

// toolTableShareDir is the root-relative (cncd) / user-scope-relative
// (filebrowser) folder tool-table JSON dumps live under. Matches
// http/cnc.go's toolTableShareDir constant.
const toolTableShareDir = "cnc-tool-tables"

// ensureLeading normalizes a client-supplied path to start with "/"
// before it's path.Clean'd. Mirrors http/cnc.go's helper of the same
// name so escape-detection behaves identically on both hosts.
func ensureLeading(p string) string {
	if strings.HasPrefix(p, "/") {
		return p
	}
	return "/" + p
}

// CleanScopedPath validates and normalizes a client-supplied
// CNC-relative path: non-empty, and unable to escape the resolver's
// jail via "..". Returns the cleaned, leading-slash path ready to
// hand to a PathResolver. Mirrors the file_path validation inlined at
// the top of nearly every handler in http/cnc*.go.
func CleanScopedPath(raw string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("file_path required")
	}
	clean := path.Clean(ensureLeading(raw))
	if strings.Contains(clean, "..") {
		return "", fmt.Errorf("file_path must not escape the share")
	}
	return clean, nil
}

// SanitizeMachineID strips path separators and traversal chars from a
// machine ID so a malicious settings entry can't escape the tool-table
// dump dir. Mirrors http/cnc.go's sanitizeMachineID.
func SanitizeMachineID(id string) string {
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

// FindMachine returns the settings.Machine matching id in cfg, or
// cfg.Machines[0] when id is empty. nil if cfg has no machines, or id
// is non-empty and unmatched. Mirrors http/cnc.go's findMachine. cfg
// is a value (typically from SettingsStore.Snapshot()), so the
// returned pointer aliases that local copy, never live settings state.
func FindMachine(cfg settings.Cnc, id string) *settings.Machine {
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

// DefaultToolSlotsForMachine looks up id's configured ToolSlots, or
// settings.DefaultToolSlots when the machine isn't found. Mirrors
// http/cnc.go's defaultToolSlotsForMachine.
func DefaultToolSlotsForMachine(cfg settings.Cnc, id string) int {
	if m, ok := cfg.MachineByID(id); ok {
		return m.EffectiveToolSlots()
	}
	return settings.DefaultToolSlots
}

// ResolveStreamer pulls the streamer for machineID (empty = registry
// default). status/err are non-zero/non-nil (404) when no machine
// matches — the (status, err) shape matches every http/cnc*.go
// handler's own return, so callers there can return it directly.
func (d Deps) ResolveStreamer(machineID string) (st *cnc.Streamer, resolvedID string, status int, err error) {
	st, resolvedID = d.Registry.Streamer(machineID)
	if st == nil {
		return nil, "", 404, fmt.Errorf("no machine configured (id=%q)", machineID)
	}
	return st, resolvedID, 0, nil
}

// ResolveAggregator mirrors ResolveStreamer for the Aggregator side.
func (d Deps) ResolveAggregator(machineID string) (ag *cnc.Aggregator, resolvedID string, status int, err error) {
	ag, resolvedID = d.Registry.Aggregator(machineID)
	if ag == nil {
		return nil, "", 404, fmt.Errorf("no machine configured (id=%q)", machineID)
	}
	return ag, resolvedID, 0, nil
}

// ToolTableDirAbs resolves the absolute tool-table dump directory for
// a machine through the seam's PathResolver. Mirrors http/cnc.go's
// toolTableDirAbs.
func (d Deps) ToolTableDirAbs(machineID string) (string, error) {
	rel := path.Join(toolTableShareDir, SanitizeMachineID(machineID))
	return d.Resolver.FullPath(rel)
}

// NewestJSONIn returns the most recently modified *.json file
// directly inside dir, or "" (no error) when dir doesn't exist yet.
// Mirrors http/cnc.go's newestJSONIn.
func NewestJSONIn(dir string) (string, error) {
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

// LatestToolTable loads the most recently persisted tool-table dump
// for machineID, or (nil, nil) when none exists yet (no dump, or a
// corrupt one — a bad dump on disk shouldn't fail every subsequent
// call, it should just look like "no history").
func (d Deps) LatestToolTable(machineID string) (*cnc.ToolTable, error) {
	dir, err := d.ToolTableDirAbs(machineID)
	if err != nil {
		return nil, err
	}
	latest, err := NewestJSONIn(dir)
	if err != nil || latest == "" {
		return nil, err
	}
	buf, err := os.ReadFile(latest)
	if err != nil {
		return nil, nil //nolint:nilerr // best-effort: treat an unreadable dump as "no table"
	}
	var t cnc.ToolTable
	if err := json.Unmarshal(buf, &t); err != nil {
		return nil, nil //nolint:nilerr // best-effort: treat a corrupt dump as "no table"
	}
	return &t, nil
}

// ReadToolTableDump reads and parses one named dump (a basename) out
// of machineID's history folder. Mirrors http/cnc.go's
// readToolTableDump, generalized to resolve the directory itself.
func (d Deps) ReadToolTableDump(machineID, name string) (*cnc.ToolTable, error) {
	dir, err := d.ToolTableDirAbs(machineID)
	if err != nil {
		return nil, err
	}
	buf, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return nil, err
	}
	var t cnc.ToolTable
	if err := json.Unmarshal(buf, &t); err != nil {
		return nil, fmt.Errorf("parse %s: %w", name, err)
	}
	return &t, nil
}

// CurrentSpindleTool reads the aggregator's "tool" metric (Q201) for
// machineID and parses it to an int. Returns nil when the metric is
// unavailable, stale, or unparseable — callers treat nil as "swap
// unknown". Mirrors http/cnc.go's currentSpindleTool.
func (d Deps) CurrentSpindleTool(machineID string) *int {
	ag, _ := d.Registry.Aggregator(machineID)
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

// PersistToolTable writes tbl as <RFC3339>.json into machineID's
// tool-table dump folder, creating it if needed. Mirrors
// http/cnc.go's persistToolTable.
func (d Deps) PersistToolTable(machineID string, tbl *cnc.ToolTable) error {
	dir, err := d.ToolTableDirAbs(machineID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir tool-table dir: %w", err)
	}
	// Filesystem-safe RFC3339 — colons aren't legal on FAT/exFAT.
	stamp := tbl.ReadAt.UTC().Format("2006-01-02T15-04-05Z")
	name := stamp + ".json"
	full := filepath.Join(dir, name)
	buf, err := json.MarshalIndent(tbl, "", "  ")
	if err != nil {
		return err
	}
	tmp := full + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, full)
}
