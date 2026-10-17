package cncd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/filebrowser/filebrowser/v2/cnc"
	"github.com/filebrowser/filebrowser/v2/cncapi"
	"github.com/filebrowser/filebrowser/v2/settings"
)

type fakeSetupSource struct {
	running, up bool
	program     string
	busy        bool
	spindle     int
	err         error
	// length/diameter the control "shows" per pocket for partial reads
	live  map[int][2]float64
	reads [][]int
}

func (f *fakeSetupSource) IsRunning() bool { return f.running }
func (f *fakeSetupSource) LinkUp() bool    { return f.up }
func (f *fakeSetupSource) ProgramState(context.Context) (string, bool, error) {
	return f.program, f.busy, f.err
}
func (f *fakeSetupSource) ToolInSpindle(context.Context) (int, error) { return f.spindle, nil }
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
	r.src.program = "MDI" // MDI leaves the program in memory
	r.step()
	if s := r.sheets.Sheet("m1"); s.State != cnc.SheetNoJSON || s.Program != "31011" {
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
	var files []string
	for _, f := range r.dumpFiles() {
		if strings.HasSuffix(f, ".json") {
			files = append(files, f)
		}
	}
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
	r.src.program = "O99999" // another program loaded
	r.step()
	if s := r.sheets.Sheet("m1"); s.Program != "99999" {
		t.Fatalf("expected the new program, got %+v", s)
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
	// With a sheet up, "updated" is the last poll (setupNow, 02:00Z), not
	// the dump's read_at (01:20Z): 22:00 the evening before in New York.
	if body.Data.LastUpdatedDisplay != "2026-10-16 22:00" {
		t.Fatalf("display time %q", body.Data.LastUpdatedDisplay)
	}
	if len(body.Data.Pockets) == 0 {
		t.Fatal("existing tool-list payload must be unchanged")
	}
	if body.Setup.State != cnc.SheetReady || len(body.Setup.Tools) != 2 || body.Setup.Tools[1].Shape == nil {
		t.Fatalf("setup: %+v", body.Setup)
	}
}

func TestSetupSheetCheckedAtIsTheLastPoll(t *testing.T) {
	r := newSetupRig(t)
	if !r.sheets.CheckedAt("m1").IsZero() {
		t.Fatal("checked before the first poll")
	}
	r.step()
	if got := r.sheets.CheckedAt("m1"); !got.Equal(setupNow) {
		t.Fatalf("CheckedAt = %v, want %v", got, setupNow)
	}
}

// A deploy restarts cncd mid-setup. The "before" values survive, so a
// touch-off made across the restart still clears the pocket.
func TestSetupSheetBaselineSurvivesRestart(t *testing.T) {
	r := newSetupRig(t)
	r.src.live[15] = [2]float64{8.4096, 0.3758}
	r.step()
	if r.status(15) != cnc.StatusTouch {
		t.Fatal("T15 should be TOUCH")
	}

	restarted := NewSetupSheets(r.d)
	restarted.now = func() time.Time { return setupNow }
	restarted.src = func(string) setupSource { return r.src }
	r.src.live[15] = [2]float64{9.1234, 0.3758} // touched off while cncd was down
	restarted.Step(context.Background(), "m1")
	for _, tl := range restarted.Sheet("m1").Tools {
		if tl.Pocket == 15 && tl.Status != cnc.StatusOK {
			t.Fatalf("touch-off across a restart was lost: %s", tl.Status)
		}
	}
}

// Jason re-touched T18 and landed on the same length; he changed the
// diameter offset. Any offset change is the touch-off.
func TestTouchedOffOnDiameterChange(t *testing.T) {
	ball := cnc.CardTool{Number: 18, Type: "ball end mill", Diameter: 0.1575}
	l, d0, d1 := 3.4254, 0.1604, 0.1613
	before := cnc.PocketGeom{LengthGeom: &l, DiameterGeom: &d0}
	now := cnc.PocketGeom{LengthGeom: &l, DiameterGeom: &d1}
	if !cnc.TouchedOff(ball, now, before) {
		t.Fatal("a diameter offset change is a touch-off")
	}
	if cnc.TouchedOff(ball, before, before) {
		t.Fatal("nothing changed")
	}
}

// Touch-offs are always macros. A re-touch can land on the same length,
// so the macro itself (O9000+ running with the tool in the spindle) is the
// touch-off: T15 records at its unchanged length when the macro ends.
func TestSetupSheetTouchOffMacroRecordsSameLength(t *testing.T) {
	r := newSetupRig(t)
	r.src.live[15] = [2]float64{8.4096, 0.3758}
	r.step()
	if r.status(15) != cnc.StatusTouch {
		t.Fatal("T15 should be TOUCH")
	}

	r.src.program, r.src.busy, r.src.spindle = "O9023", true, 15
	r.sheets.poll(context.Background(), "m1", false)
	if s := r.sheets.Sheet("m1"); s.Program != "31011" {
		t.Fatalf("a running macro must not replace the program: %q", s.Program)
	}

	r.src.program, r.src.busy, r.src.spindle = "MDI", false, 15
	r.sheets.poll(context.Background(), "m1", false)
	if s := r.sheets.Sheet("m1"); s.Program != "31011" {
		t.Fatalf("MDI keeps the program in memory: %q", s.Program)
	}
	if got := r.status(15); got != cnc.StatusOK {
		t.Fatalf("after the touch-off macro, same length: %s", got)
	}
	m, _, _ := cnc.LoadManifest(filepath.Join(r.ttDir, cnc.ManifestFileName))
	if e, ok := m["15"]; !ok || e.LengthGeom != 8.4096 {
		t.Fatalf("manifest T15: %+v", m["15"])
	}
}

// Mid-cycle #4115 can name a subprogram; the job's program stays.
func TestSetupSheetBusySubprogramKeepsProgram(t *testing.T) {
	r := newSetupRig(t)
	r.step()
	r.src.program, r.src.busy = "O1234", true
	r.step()
	if s := r.sheets.Sheet("m1"); s.Program != "31011" {
		t.Fatalf("busy subprogram replaced the program: %q", s.Program)
	}
}

// The post moved the sidecar into 31011/ beside a setup sheet: the sheet
// still builds, the display gets a QR of /setup/31011 on the host it used,
// and that address lands on the setup sheet.
func TestSetupSheetFolderAndQR(t *testing.T) {
	r := newSetupRig(t)
	folder := filepath.Join(r.d.Root, "31011")
	if err := os.Mkdir(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(r.d.Root, "31011.json"), filepath.Join(folder, "31011.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, "31011 setup.html"), []byte("<h1>sheet</h1>"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.step()
	r.d.Setup = r.sheets
	if err := r.d.Config.Update(func(c *settings.Cnc) error {
		c.Displays = []settings.Display{{ID: "d1", MachineID: "m1"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	h := NewRouter(r.d)
	req := httptest.NewRequest(http.MethodGet, "/api/displays/d1", nil)
	req.Host = "192.168.20.11:8080"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var body struct {
		Setup cnc.SetupSheet `json:"setup"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Setup.State != cnc.SheetReady {
		t.Fatalf("sidecar in the folder not found: %+v", body.Setup)
	}
	qr := body.Setup.QR
	if qr == nil || qr.URL != "http://192.168.20.11:8080/setup/31011" {
		t.Fatalf("qr %+v", qr)
	}
	n := len(qr.Rows)
	if n < 21 || len(qr.Rows[0]) != (n+3)/4 {
		t.Fatalf("qr is %d rows of %d hex", n, len(qr.Rows[0]))
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/setup/31011", nil))
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/setup/31011/31011%20setup.html" {
		t.Fatalf("redirect %d %q", rec.Code, rec.Header().Get("Location"))
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/setup/31011/31011%20setup.html", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "sheet") {
		t.Fatalf("sheet %d %q", rec.Code, rec.Body)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/setup/31011/../31011.nc", nil))
	if rec.Code == 200 && strings.Contains(rec.Body.String(), "O31011") {
		t.Fatal("served a file outside the folder")
	}

	// No setup sheet: no QR, and the address says so.
	if err := os.Remove(filepath.Join(folder, "31011 setup.html")); err != nil {
		t.Fatal(err)
	}
	if qr := r.d.setupQR("31011", "h"); qr != nil {
		t.Fatalf("qr without a sheet: %+v", qr)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/setup/31011", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("no sheet -> %d", rec.Code)
	}
}

func TestQRRowsPackMSBFirst(t *testing.T) {
	got := qrRows([][]bool{{true, false, false, false, true}, {false, true, true, true, true}})
	if got[0] != "88" || got[1] != "78" {
		t.Fatalf("%v", got)
	}
}
