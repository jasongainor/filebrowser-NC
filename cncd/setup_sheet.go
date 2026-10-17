package cncd

// Setup sheet: the loaded program's tool list, with each pocket's status
// against the machine, for the e-ink display (the "setup" key of
// GET /api/displays/{id}).
//
// Once a minute, through the Link and without waking the 3 s aggregator,
// cncd asks the control which program is loaded (Q500), finds that
// program's newest .nc in the share root by the O number inside the file,
// and reads its <base>.json sidecar. While any pocket of the program is not
// "ok" it re-reads just those pockets (Q600 on their offset variables) and,
// when the machine shows the operator touched a tool off, records the
// program's tool identity and the new length in manifest.json. That is the
// only manifest writer; there is no confirm step.

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/filebrowser/filebrowser/v2/cnc"
	"github.com/filebrowser/filebrowser/v2/cncapi"
)

const (
	setupPollInterval = 60 * time.Second
	setupFirstPoll    = 5 * time.Second
	// setupTick is how often the loaded program is asked (one Q500, plus
	// #4115 mid-cycle): fast enough to catch a touch-off macro, which runs
	// for tens of seconds. The rest of a poll runs every setupPollInterval.
	setupTick = 5 * time.Second
	// O09000-O09999 are macros (touch-off, probing), never the job's
	// program. Job programs go well past them (31011).
	macroFloor, macroCeil = 9000, 9999
	setupQueryTimeout     = 10 * time.Second
	setupReadTimeout      = 2 * time.Minute
)

// setupSource is the slice of *cnc.Streamer the sheet needs; a seam so
// tests run without a bridge.
type setupSource interface {
	IsRunning() bool
	LinkUp() bool
	ProgramState(ctx context.Context) (string, bool, error)
	ToolInSpindle(ctx context.Context) (int, error)
	ReadToolSlots(ctx context.Context, pockets []int) ([]cnc.ToolTableSlot, error)
}

type overlaySlot struct {
	geom cnc.PocketGeom
	at   time.Time
}

// setupMachine is the per-machine state.
type setupMachine struct {
	program    string
	ncPath     string
	sidecar    string
	sidecarMod time.Time
	cards      *cnc.Cards

	// overlay holds partial reads, newer than the last full dump; they are
	// never written out as dump files.
	overlay map[int]overlaySlot
	// baseline is what the machine showed for a flagged pocket when it was
	// first flagged, to tell a touch-off from no change.
	baseline map[int]cnc.PocketGeom

	manifest     cnc.Manifest
	manifestPath string
	manifestOK   bool

	// checkedAt is the last poll that read the loaded program: the time
	// the sheet on the display is current as of.
	checkedAt time.Time

	// statePath keeps program + baseline across restarts, so a deploy
	// mid-setup doesn't swallow a touch-off. A dot-file without .json so
	// the dump listing never mistakes it for a tool table.
	statePath     string
	savedProgram  string
	savedBaseline map[int]cnc.PocketGeom
	baselineDirty bool

	// A touch-off macro (O9000+) seen running, and the tools that were in
	// the spindle while it ran. When it ends those pockets are recorded as
	// touched off, even if the length came out the same.
	inMacro    bool
	macroTools map[int]bool
	lastFull   time.Time
}

// setupState is what statePath holds.
type setupState struct {
	Program  string                    `json:"program"`
	Baseline map[string]cnc.PocketGeom `json:"baseline"`
}

func (m *setupMachine) loadState() {
	buf, err := os.ReadFile(m.statePath)
	if err != nil {
		return
	}
	var st setupState
	if json.Unmarshal(buf, &st) != nil {
		return
	}
	m.savedProgram = st.Program
	m.savedBaseline = map[int]cnc.PocketGeom{}
	for k, g := range st.Baseline {
		if p, err := strconv.Atoi(k); err == nil {
			m.savedBaseline[p] = g
		}
	}
}

func (m *setupMachine) saveState() {
	if m.statePath == "" {
		return
	}
	st := setupState{Program: m.program, Baseline: map[string]cnc.PocketGeom{}}
	for p, g := range m.baseline {
		st.Baseline[strconv.Itoa(p)] = g
	}
	buf, err := json.Marshal(st)
	if err != nil {
		return
	}
	tmp := m.statePath + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o644); err != nil {
		log.Printf("cncd: setup sheet state: %v", err)
		return
	}
	if err := os.Rename(tmp, m.statePath); err != nil {
		log.Printf("cncd: setup sheet state: %v", err)
	}
}

// SetupSheets owns the setup-sheet state for every machine.
type SetupSheets struct {
	d   Deps
	now func() time.Time
	src func(id string) setupSource

	mu sync.Mutex
	ms map[string]*setupMachine
}

// NewSetupSheets wires the sheet to the live registry.
func NewSetupSheets(d Deps) *SetupSheets {
	return &SetupSheets{
		d:   d,
		now: time.Now,
		src: func(id string) setupSource {
			st, _ := d.Registry.Streamer(id)
			if st == nil {
				return nil
			}
			return streamerAdapter{st}
		},
		ms: map[string]*setupMachine{},
	}
}

// Run polls every machine once a minute until ctx is done.
func (s *SetupSheets) Run(ctx context.Context) {
	t := time.NewTimer(setupFirstPoll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		for _, m := range s.d.Registry.Machines() {
			s.poll(ctx, m.ID, false)
		}
		t.Reset(setupTick)
	}
}

// Sheet is the current payload for a machine ("" = the default one).
// Safe on a nil receiver (hosts that don't run the poller).
func (s *SetupSheets) Sheet(machineID string) cnc.SetupSheet {
	if s == nil {
		return cnc.SetupSheet{State: cnc.SheetNone}
	}
	cfg := s.d.Config.Snapshot()
	m := cncapi.FindMachine(cfg, machineID)
	if m == nil {
		return cnc.SetupSheet{State: cnc.SheetNone}
	}
	tbl, _ := cncapiDepsFor(s.d).LatestToolTable(m.ID)
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.ms[m.ID]
	if st == nil {
		return cnc.SetupSheet{State: cnc.SheetNone}
	}
	return s.sheetLocked(st, tbl)
}

// CheckedAt is when the sheet for a machine was last refreshed from the
// control (zero before the first poll). Safe on a nil receiver.
func (s *SetupSheets) CheckedAt(machineID string) time.Time {
	if s == nil {
		return time.Time{}
	}
	m := cncapi.FindMachine(s.d.Config.Snapshot(), machineID)
	if m == nil {
		return time.Time{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.ms[m.ID]; st != nil {
		return st.checkedAt
	}
	return time.Time{}
}

// Step runs one full poll for a machine: loaded program, sidecar, partial
// re-read of non-ok pockets, manifest recording.
func (s *SetupSheets) Step(ctx context.Context, machineID string) { s.poll(ctx, machineID, true) }

// poll asks the control for the loaded program every tick. A touch-off
// macro running (busy, O9000+) only notes the tool in the spindle; the
// full poll runs when forced, when a macro just ended, when the program
// changed, or once a setupPollInterval.
func (s *SetupSheets) poll(ctx context.Context, machineID string, force bool) {
	src := s.src(machineID)
	if src == nil || src.IsRunning() || !src.LinkUp() {
		return
	}
	qctx, cancel := context.WithTimeout(ctx, setupQueryTimeout)
	token, busy, err := src.ProgramState(qctx)
	cancel()
	if err != nil {
		log.Printf("cncd: setup sheet %s: Q500: %v", machineID, err)
		return
	}
	number := cnc.LoadedProgramNumber(token)
	n, _ := strconv.Atoi(number)
	macro := n >= macroFloor && n <= macroCeil

	if macro && busy {
		qctx, cancel := context.WithTimeout(ctx, setupQueryTimeout)
		tool, terr := src.ToolInSpindle(qctx)
		cancel()
		s.mu.Lock()
		m := s.machineLocked(machineID)
		if !m.inMacro {
			log.Printf("cncd: setup sheet %s: macro O%s running", machineID, number)
		}
		m.inMacro = true
		if m.macroTools == nil {
			m.macroTools = map[int]bool{}
		}
		if terr == nil && tool > 0 && !m.macroTools[tool] {
			m.macroTools[tool] = true
			log.Printf("cncd: setup sheet %s: macro O%s with T%d in the spindle", machineID, number, tool)
		}
		s.mu.Unlock()
		return
	}

	tbl, _ := cncapiDepsFor(s.d).LatestToolTable(machineID)
	s.mu.Lock()
	m := s.machineLocked(machineID)
	// MDI, or a macro loaded but not running, leaves the job's program in
	// memory; and mid-cycle #4115 can name a subprogram, while no new
	// program can be loaded. Keep the program we have in those cases.
	if number == "" || macro || (busy && m.program != "") {
		number = m.program
	}
	touched := m.macroTools
	ended := m.inMacro
	m.inMacro, m.macroTools = false, nil
	if !(force || ended || number != m.program || s.now().Sub(m.lastFull) >= setupPollInterval) {
		s.mu.Unlock()
		return
	}
	s.syncProgramLocked(machineID, m, number)
	need := s.nonOKLocked(m, tbl)
	need = append(need, s.touchedPocketsLocked(m, touched)...)
	s.mu.Unlock()

	var slots []cnc.ToolTableSlot
	if len(need) > 0 {
		rctx, cancel := context.WithTimeout(ctx, setupReadTimeout)
		slots, err = src.ReadToolSlots(rctx, need)
		cancel()
		if err != nil {
			log.Printf("cncd: setup sheet %s: partial read: %v", machineID, err)
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	m.checkedAt = now
	m.lastFull = now
	for _, sl := range slots {
		m.overlay[sl.Slot] = overlaySlot{geom: cnc.GeomOf(sl), at: now}
	}
	s.recordTouchedLocked(machineID, m, tbl, now, touched)
	s.recordLocked(machineID, m, tbl, now)
}

// touchedPocketsLocked is the program's floating pockets a macro touched
// off, to re-read alongside the non-ok ones (duplicates are harmless).
func (s *SetupSheets) touchedPocketsLocked(m *setupMachine, touched map[int]bool) []int {
	if m.cards == nil || len(touched) == 0 {
		return nil
	}
	var out []int
	for _, t := range m.cards.ProgramTools() {
		if touched[t.Number] && !cnc.IsFixedPocket(t.Number) {
			out = append(out, t.Number)
		}
	}
	return out
}

// recordTouchedLocked records each program tool a touch-off macro just
// measured, at the length the machine now shows.
func (s *SetupSheets) recordTouchedLocked(id string, m *setupMachine, tbl *cnc.ToolTable, now time.Time, touched map[int]bool) {
	if m.cards == nil || len(touched) == 0 || !m.manifestOK {
		return
	}
	dirty := false
	for _, t := range m.cards.ProgramTools() {
		p := t.Number
		if !touched[p] || cnc.IsFixedPocket(p) {
			continue
		}
		g := m.geomLocked(tbl, p)
		if g == nil {
			continue
		}
		l, ok := g.Length()
		if !ok || l == 0 {
			continue
		}
		m.manifest[strconv.Itoa(p)] = cnc.RecordFor(t, l, now.UTC().Format(time.RFC3339))
		if _, flagged := m.baseline[p]; flagged {
			delete(m.baseline, p)
			m.baselineDirty = true
		}
		dirty = true
		log.Printf("cncd: setup sheet %s: T%d touched off by macro, recorded as %s %.4f, length %.4f", id, p, t.Type, t.Diameter, l)
	}
	if dirty {
		if err := m.manifest.Save(m.manifestPath); err != nil {
			log.Printf("cncd: setup sheet %s: manifest save: %v", id, err)
		}
	}
}

// machineLocked returns the state for id, loading (and migrating) the
// manifest on first use.
func (s *SetupSheets) machineLocked(id string) *setupMachine {
	if m := s.ms[id]; m != nil {
		return m
	}
	m := &setupMachine{overlay: map[int]overlaySlot{}, baseline: map[int]cnc.PocketGeom{}, manifest: cnc.Manifest{}}
	if dir, err := cncapiDepsFor(s.d).ToolTableDirAbs(id); err == nil {
		m.manifestPath = filepath.Join(dir, cnc.ManifestFileName)
		m.statePath = filepath.Join(dir, ".setup-state")
		m.loadState()
		mf, migrated, err := cnc.LoadManifest(m.manifestPath)
		switch {
		case err != nil:
			log.Printf("cncd: setup sheet %s: manifest unreadable, not recording: %v", id, err)
		default:
			m.manifest, m.manifestOK = mf, true
			if migrated {
				if err := mf.Save(m.manifestPath); err != nil {
					log.Printf("cncd: setup sheet %s: manifest migration save: %v", id, err)
				} else {
					log.Printf("cncd: setup sheet %s: migrated guid-keyed manifest to tool identity", id)
				}
			}
		}
	}
	s.ms[id] = m
	return m
}

// syncProgramLocked points m at the newest file for number and its sidecar.
func (s *SetupSheets) syncProgramLocked(id string, m *setupMachine, number string) {
	if number != m.program {
		log.Printf("cncd: setup sheet %s: loaded program %q -> %q", id, m.program, number)
		restore := m.program == "" && number != "" && number == m.savedProgram
		m.program = number
		m.overlay = map[int]overlaySlot{}
		m.baseline = map[int]cnc.PocketGeom{}
		if restore {
			// cncd restarted with this program still loaded: keep the
			// "before" values so a touch-off made meanwhile still counts.
			for p, g := range m.savedBaseline {
				m.baseline[p] = g
			}
		}
		m.savedProgram, m.savedBaseline = "", nil
		m.baselineDirty = true
		m.ncPath, m.sidecar, m.sidecarMod, m.cards = "", "", time.Time{}, nil
	}
	if number == "" {
		return
	}
	pf, err := cnc.FindProgramFile(s.d.Root, number)
	if err != nil || pf == nil || pf.Sidecar == "" {
		m.ncPath, m.sidecar, m.sidecarMod, m.cards = "", "", time.Time{}, nil
		return
	}
	info, err := os.Stat(pf.Sidecar)
	if err != nil {
		m.cards = nil
		return
	}
	if pf.NC == m.ncPath && pf.Sidecar == m.sidecar && info.ModTime().Equal(m.sidecarMod) && m.cards != nil {
		return
	}
	m.ncPath, m.sidecar, m.sidecarMod, m.cards = pf.NC, pf.Sidecar, info.ModTime(), nil
	buf, err := os.ReadFile(pf.Sidecar)
	if err == nil {
		m.cards, err = cnc.ParseCards(buf)
	}
	if err != nil {
		log.Printf("cncd: setup sheet %s: %s: %v", id, filepath.Base(pf.Sidecar), err)
		m.cards = nil
	}
}

// geomLocked is what the machine shows for pocket: the last full dump,
// overlaid with any newer partial read.
func (m *setupMachine) geomLocked(tbl *cnc.ToolTable, pocket int) *cnc.PocketGeom {
	var g *cnc.PocketGeom
	var readAt time.Time
	if tbl != nil {
		readAt = tbl.ReadAt
		for _, sl := range tbl.Slots {
			if sl.Slot == pocket {
				gg := cnc.GeomOf(sl)
				g = &gg
				break
			}
		}
	}
	if o, ok := m.overlay[pocket]; ok && o.at.After(readAt) {
		if g == nil {
			g = &cnc.PocketGeom{}
		}
		if o.geom.LengthGeom != nil {
			g.LengthGeom, g.LengthWear = o.geom.LengthGeom, o.geom.LengthWear
		}
		if o.geom.DiameterGeom != nil {
			g.DiameterGeom, g.DiameterWear = o.geom.DiameterGeom, o.geom.DiameterWear
		}
	}
	return g
}

func (m *setupMachine) recLocked(pocket int) *cnc.ManifestEntry {
	if e, ok := m.manifest[strconv.Itoa(pocket)]; ok {
		return &e
	}
	return nil
}

func (s *SetupSheets) sheetLocked(m *setupMachine, tbl *cnc.ToolTable) cnc.SetupSheet {
	return cnc.BuildSetupSheet(m.program, m.cards,
		func(p int) *cnc.PocketGeom { return m.geomLocked(tbl, p) },
		m.recLocked)
}

// nonOKLocked lists the program's pockets that are not ok.
func (s *SetupSheets) nonOKLocked(m *setupMachine, tbl *cnc.ToolTable) []int {
	if m.cards == nil {
		return nil
	}
	var out []int
	for _, t := range m.cards.ProgramTools() {
		if cnc.PocketStatus(t, m.geomLocked(tbl, t.Number), m.recLocked(t.Number)) != cnc.StatusOK {
			out = append(out, t.Number)
		}
	}
	return out
}

// recordLocked walks the program's floating pockets. A pocket first seen
// not-ok gets a baseline. A flagged pocket that is now ok, or a drill/tap
// whose length changed since it was flagged, has been set up by the
// operator: record the program's tool and the machine's length.
func (s *SetupSheets) recordLocked(id string, m *setupMachine, tbl *cnc.ToolTable, now time.Time) {
	if m.cards == nil {
		return
	}
	inProgram := map[int]bool{}
	dirty := false
	for _, t := range m.cards.ProgramTools() {
		p := t.Number
		inProgram[p] = true
		if cnc.IsFixedPocket(p) {
			continue
		}
		g := m.geomLocked(tbl, p)
		status := cnc.PocketStatus(t, g, m.recLocked(p))
		before, flagged := m.baseline[p]
		switch {
		case status == cnc.StatusOK && !flagged && g != nil && cnc.RecordStale(t, lengthOf(g), m.recLocked(p)):
			// touched off since the record: record the program's tool at the new length
			l := lengthOf(g)
			m.manifest[strconv.Itoa(p)] = cnc.RecordFor(t, l, now.UTC().Format(time.RFC3339))
			if m.manifestOK {
				dirty = true
			}
			log.Printf("cncd: setup sheet %s: T%d re-recorded as %s %.4f, length %.4f", id, p, t.Type, t.Diameter, l)
		case status == cnc.StatusOK && !flagged:
			// fine all along
		case status != cnc.StatusOK && !flagged:
			if g != nil {
				m.baseline[p] = *g
			} else {
				m.baseline[p] = cnc.PocketGeom{}
			}
			m.baselineDirty = true
		case g != nil && (status == cnc.StatusOK || cnc.TouchedOff(t, *g, before)):
			if l, ok := g.Length(); ok && l != 0 && m.manifestOK {
				m.manifest[strconv.Itoa(p)] = cnc.RecordFor(t, l, now.UTC().Format(time.RFC3339))
				dirty = true
				log.Printf("cncd: setup sheet %s: T%d set up as %s %.4f, length %.4f", id, p, t.Type, t.Diameter, l)
			}
			delete(m.baseline, p)
			m.baselineDirty = true
		}
	}
	for p := range m.baseline {
		if !inProgram[p] {
			delete(m.baseline, p)
			m.baselineDirty = true
		}
	}
	if m.baselineDirty {
		m.saveState()
		m.baselineDirty = false
	}
	if dirty {
		if err := m.manifest.Save(m.manifestPath); err != nil {
			log.Printf("cncd: setup sheet %s: manifest save: %v", id, err)
		}
	}
}

func lengthOf(g *cnc.PocketGeom) float64 {
	l, _ := g.Length()
	return l
}
