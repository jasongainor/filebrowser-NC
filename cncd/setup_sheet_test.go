package cncd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/filebrowser/filebrowser/v2/cnc"
	"github.com/filebrowser/filebrowser/v2/cncapi"
	"github.com/filebrowser/filebrowser/v2/settings"
)

type fakeSetupSource struct {
	running, up bool
	program     string
	err         error
	// length/diameter the control "shows" per pocket for partial reads
	live  map[int][2]float64
	reads [][]int
}

func (f *fakeSetupSource) IsRunning() bool { return f.running }
func (f *fakeSetupSource) LinkUp() bool    { return f.up }
func (f *fakeSetupSource) LoadedProgram(context.Context) (string, error) {
	return f.program, f.err
}
func (f *fakeSetupSource) ReadToolSlots(_ context.Context, pockets []int) ([]cnc.ToolTableSlot, error) {
	f.reads = append(f.reads, append([]int(nil), pockets...))
	var out []cnc.ToolTableSlot
	for _, p := range pockets {
		v := f.live[p]
		l, d, w := v[0], v[1], 0.0
		out = append(out, cnc.ToolTableSlot{Slot: p, LengthGeom: &l, DiameterGeom: &d, DiameterWear: &w})
	}
	return out, nil
}

var setupNow = time.Date(2026, 10, 17, 2, 0, 0, 0, time.UTC)

// setupRig is a cncd with the fixture program 31011 in its root, the
// fixture tool table as the latest dump, and a fake control.
type setupRig struct {
	t       *testing.T
	d       Deps
	sheets  *SetupSheets
	src     *fakeSetupSource
	ttDir   string
	dumpPth string
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	buf, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, buf, 0o644); err != nil {
		t.Fatal(err)
	}
}

func newSetupRig(t *testing.T) *setupRig {
	t.Helper()
	root := t.TempDir()
	d, _ := newTestDepsWithRoot(t, root, nil)
	copyFile(t, "../cnc/testdata/31011.nc", filepath.Join(root, "31011.nc"))
	copyFile(t, "../cnc/testdata/31011.json", filepath.Join(root, "31011.json"))
	ttDir, err := cncapiDepsFor(d).ToolTableDirAbs("m1")
	if err != nil {
		t.Fatal(err)
	}
	dump := filepath.Join(ttDir, "2026-10-17T01-20-31Z.json")
	copyFile(t, "../cnc/testdata/tooltable-primary.json", dump)

	src := &fakeSetupSource{up: true, program: "O31011", live: map[int][2]float64{}}
	s := NewSetupSheets(d)
	s.now = func() time.Time { return setupNow }
	s.src = func(string) setupSource { return src }
	return &setupRig{t: t, d: d, sheets: s, src: src, ttDir: ttDir, dumpPth: dump}
}

func (r *setupRig) status(pocket int) string {
	r.t.Helper()
	for _, tl := range r.sheets.Sheet("m1").Tools {
		if tl.Pocket == pocket {
			return tl.Status
		}
	}
	r.t.Fatalf("pocket %d not on the sheet", pocket)
	return ""
}

func (r *setupRig) step() { r.sheets.Step(context.Background(), "m1") }

func (r *setupRig) dumpFiles() []string {
	ents, _ := os.ReadDir(r.ttDir)
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

func TestSetupSheetLoadedProgramFromFixture(t *testing.T) {
	r := newSetupRig(t)
	if got := r.sheets.Sheet("m1").State; got != cnc.SheetNone {
		t.Fatalf("before the first poll: %s", got)
	}
	r.src.live[15] = [2]float64{8.4096, 0.3758}
	r.step()
	s := r.sheets.Sheet("m1")
	if s.State != cnc.SheetReady || s.Program != "31011" || len(s.Tools) != 2 {
		t.Fatalf("%+v", s)
	}
	if s.Tools[0].Pocket != 14 || s.Tools[0].Status != cnc.StatusOK {
		t.Fatalf("T14 (fixed chamfer, dia .3772 vs .375): %+v", s.Tools[0])
	}
	if s.Tools[1].Pocket != 15 || s.Tools[1].Status != cnc.StatusTouch {
		t.Fatalf("T15 1/2 drill never recorded: %+v", s.Tools[1])
	}
}

func TestSetupSheetNoJSONAndNoProgram(t *testing.T) {
	r := newSetupRig(t)
	if err := os.Remove(filepath.Join(r.d.Root, "31011.json")); err != nil {
		t.Fatal(err)
	}
	r.step()
	if s := r.sheets.Sheet("m1"); s.State != cnc.SheetNoJSON || s.Program != "31011" || len(s.Tools) != 0 {
		t.Fatalf("no sidecar: %+v", s)
	}
	r.src.program = "MDI"
	r.step()
	if s := r.sheets.Sheet("m1"); s.State != cnc.SheetNone {
		t.Fatalf("MDI: %+v", s)
	}
	r.src.program = "O99999" // not on disk
	r.step()
	if s := r.sheets.Sheet("m1"); s.State != cnc.SheetNoJSON {
		t.Fatalf("unknown program: %+v", s)
	}
}

func TestSetupSheetSkipsWhileStreamingOrLinkDown(t *testing.T) {
	r := newSetupRig(t)
	r.src.running = true
	r.step()
	r.src.running, r.src.up = false, false
	r.step()
	if s := r.sheets.Sheet("m1"); s.State != cnc.SheetNone {
		t.Fatalf("polled while it must not: %+v", s)
	}
}

// The core promise: no confirm step. The tool is flagged, the operator
// touches it off, the next minute's partial read sees a new length, the
// manifest records the program's tool, and the status clears.
func TestSetupSheetPartialReadClearsAndRecords(t *testing.T) {
	r := newSetupRig(t)
	r.src.live[15] = [2]float64{8.4096, 0.3758} // unchanged
	r.step()
	if r.status(15) != cnc.StatusTouch {
		t.Fatal("T15 should be TOUCH")
	}
	if len(r.src.reads) != 1 || len(r.src.reads[0]) != 1 || r.src.reads[0][0] != 15 {
		t.Fatalf("only the non-ok pocket is re-read: %v", r.src.reads)
	}

	r.step() // still not touched
	if r.status(15) != cnc.StatusTouch {
		t.Fatal("unchanged length must not clear TOUCH")
	}

	r.src.live[15] = [2]float64{9.1234, 0.5} // operator touched off
	r.step()
	if got := r.status(15); got != cnc.StatusOK {
		t.Fatalf("after touch-off: %s", got)
	}
	m, _, err := cnc.LoadManifest(filepath.Join(r.ttDir, cnc.ManifestFileName))
	if err != nil {
		t.Fatal(err)
	}
	e, ok := m["15"]
	if !ok || e.Type != "drill" || e.Diameter < 0.4999 || e.Diameter > 0.5001 || e.LengthGeom != 9.1234 || e.ConfirmedAt != "2026-10-17T02:00:00Z" {
		t.Fatalf("manifest entry: %+v", e)
	}

	readsBefore := len(r.src.reads)
	r.step() // all ok now: nothing to re-read
	if len(r.src.reads) != readsBefore {
		t.Fatalf("re-read with every pocket ok: %v", r.src.reads)
	}

	// Partial reads never write a timestamped dump; the manifest does not
	// shadow the dump as "latest".
	files := r.dumpFiles()
	if len(files) != 2 {
		t.Fatalf("expected the one dump + manifest.json, got %v", files)
	}
	latest, _ := cncapi.NewestJSONIn(r.ttDir)
	if filepath.Base(latest) != "2026-10-17T01-20-31Z.json" {
		t.Fatalf("manifest.json must not count as the latest dump: %s", latest)
	}
	tbl, _ := cncapiDepsFor(r.d).LatestToolTable("m1")
	if tbl == nil || len(tbl.Slots) != 20 {
		t.Fatalf("latest table lost: %+v", tbl)
	}
}

func TestSetupSheetMillSwapClearsOnDiameter(t *testing.T) {
	r := newSetupRig(t)
	side := `{"schema":"gmw-nc-cards/1","program":{"name":"31011","comment":"c","unit":"in"},"tools":[
	 {"number":9,"type":"flat end mill","diameter":0.5,"fluteLength":1,"bodyLength":2.5}]}`
	if err := os.WriteFile(filepath.Join(r.d.Root, "31011.json"), []byte(side), 0o644); err != nil {
		t.Fatal(err)
	}
	r.src.live[9] = [2]float64{3.9272, 0.0905} // old tool still there
	r.step()
	if r.status(9) != cnc.StatusSwap {
		t.Fatalf("T9 should be SWAP, got %s", r.status(9))
	}
	r.src.live[9] = [2]float64{4.5, 0.4998} // swapped and touched off
	r.step()
	if r.status(9) != cnc.StatusOK {
		t.Fatalf("T9 should clear on matching diameter, got %s", r.status(9))
	}
	m, _, _ := cnc.LoadManifest(filepath.Join(r.ttDir, cnc.ManifestFileName))
	if e := m["9"]; e.Type != "flat end mill" || e.LengthGeom != 4.5 {
		t.Fatalf("mill not recorded: %+v", e)
	}
}

func TestSetupSheetFixedPocketNeverSwapNeverRecords(t *testing.T) {
	r := newSetupRig(t)
	side := `{"schema":"gmw-nc-cards/1","program":{"name":"31011","comment":"c","unit":"in"},"tools":[
	 {"number":3,"type":"flat end mill","diameter":0.5,"fluteLength":1,"bodyLength":2.5}]}`
	_ = os.WriteFile(filepath.Join(r.d.Root, "31011.json"), []byte(side), 0o644)
	r.src.live[3] = [2]float64{4.8743, 0.2504}
	r.step()
	if r.status(3) != cnc.StatusError {
		t.Fatalf("T3 holds the 1/4, program wants 1/2: %s", r.status(3))
	}
	r.src.live[3] = [2]float64{9, 0.5}
	r.step()
	if r.status(3) != cnc.StatusOK {
		t.Fatalf("fixed pocket clears when the right tool is there: %s", r.status(3))
	}
	if _, err := os.Stat(filepath.Join(r.ttDir, cnc.ManifestFileName)); err == nil {
		t.Fatal("fixed pockets must not be written to the manifest")
	}
}

func TestSetupSheetMigratesGuidManifest(t *testing.T) {
	r := newSetupRig(t)
	legacy := `{"15":{"guid":"7223a07f","desc":"1/2 drill","type":"drill","dc":0.5,"length_geom":8.4096,"confirmed_at":"2026-10-16T06:36:59Z"}}`
	mp := filepath.Join(r.ttDir, cnc.ManifestFileName)
	if err := os.WriteFile(mp, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	r.step()
	raw, _ := os.ReadFile(mp)
	var back map[string]map[string]any
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if _, has := back["15"]["guid"]; has {
		t.Fatalf("guid not migrated away: %s", raw)
	}
	if back["15"]["diameter"] != 0.5 {
		t.Fatalf("dc not migrated to diameter: %s", raw)
	}
	if got := r.status(15); got != cnc.StatusOK {
		t.Fatalf("migrated record (same length as the machine) should be ok, got %s", got)
	}
}

func TestSetupSheetProgramChangeResets(t *testing.T) {
	r := newSetupRig(t)
	r.src.live[15] = [2]float64{8.4096, 0.3758}
	r.step()
	r.src.program = "MDI"
	r.step()
	if r.sheets.Sheet("m1").State != cnc.SheetNone {
		t.Fatal("expected none after MDI")
	}
	r.src.program = "O31011"
	r.step()
	if r.sheets.Sheet("m1").State != cnc.SheetReady {
		t.Fatal("expected the sheet back")
	}
}

func TestDisplayPayloadCarriesSetupAndNewYorkTime(t *testing.T) {
	r := newSetupRig(t)
	r.src.live[15] = [2]float64{8.4096, 0.3758}
	r.step()
	r.d.Setup = r.sheets
	if err := r.d.Config.Update(func(c *settings.Cnc) error {
		c.Displays = []settings.Display{{ID: "d1", MachineID: "m1"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	h := NewRouter(r.d)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/displays/d1", nil))
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var body struct {
		Data struct {
			LastUpdatedDisplay string `json:"last_updated_display"`
			Pockets            []any  `json:"pockets"`
		} `json:"data"`
		Setup cnc.SetupSheet `json:"setup"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	// Dump read_at is 2026-10-17T01:20:31Z = 21:20 the evening before in New York.
	if body.Data.LastUpdatedDisplay != "2026-10-16 21:20" {
		t.Fatalf("display time %q", body.Data.LastUpdatedDisplay)
	}
	if len(body.Data.Pockets) == 0 {
		t.Fatal("existing tool-list payload must be unchanged")
	}
	if body.Setup.State != cnc.SheetReady || len(body.Setup.Tools) != 2 || body.Setup.Tools[1].Shape == nil {
		t.Fatalf("setup: %+v", body.Setup)
	}
}
