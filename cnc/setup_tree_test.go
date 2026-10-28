package cnc

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// treeDir is a share root for synthetic call trees.
type treeDir struct {
	t   *testing.T
	dir string
}

func newTreeDir(t *testing.T) *treeDir { return &treeDir{t: t, dir: t.TempDir()} }

// nc writes O<num>.nc with the body lines.
func (d *treeDir) nc(num string, lines ...string) {
	d.t.Helper()
	body := "%\nO" + num + " (prog " + num + ")\n" + strings.Join(lines, "\n") + "\n%\n"
	if err := os.WriteFile(filepath.Join(d.dir, "O"+num+".nc"), []byte(body), 0o644); err != nil {
		d.t.Fatal(err)
	}
}

// side writes the sidecar for O<num>; tools are "pocket:type:diameter".
func (d *treeDir) side(num string, tools ...string) {
	d.t.Helper()
	var ts []string
	for _, s := range tools {
		var n int
		var dia float64
		parts := strings.Split(s, ":")
		fmt.Sscan(parts[0], &n)
		fmt.Sscan(parts[2], &dia)
		ts = append(ts, fmt.Sprintf(`{"number":%d,"type":%q,"diameter":%v,"description":"d%d"}`, n, parts[1], dia, n))
	}
	body := fmt.Sprintf(`{"schema":"gmw-nc-cards/1","program":{"name":%q,"comment":"side %s","unit":"in"},"tools":[%s]}`, num, num, strings.Join(ts, ","))
	if err := os.WriteFile(filepath.Join(d.dir, "O"+num+".json"), []byte(body), 0o644); err != nil {
		d.t.Fatal(err)
	}
}

func (d *treeDir) build(main string) *ProgramTree {
	return BuildProgramTreeCached(NewParseCache(), d.dir, main)
}

func groupNumbers(tr *ProgramTree) []string {
	var out []string
	for _, g := range tr.Groups {
		out = append(out, g.Number)
	}
	return out
}

func alertsOf(tr *ProgramTree, kind string) []SetupAlert {
	var out []SetupAlert
	for _, a := range tr.Alerts {
		if a.Kind == kind {
			out = append(out, a)
		}
	}
	return out
}

func noGeom(int) *PocketGeom   { return nil }
func noRec(int) *ManifestEntry { return nil }
func treeSheet(tr *ProgramTree) SetupSheet {
	return BuildTreeSheet(tr, noGeom, noRec, NoRunPos)
}

func TestParseNCCallsAndPockets(t *testing.T) {
	info := parseNC(strings.Join([]string{
		"%", "O01090 (3+1 PROBING)",
		"(M98 P7777 inside a comment)",
		"M06 G43 T20 D20 H20",
		"T2 M06", "M06 T3", "N5 T19 M6", "T4 M06 (T9 M6)",
		"G65 P9995 W154.91", "G65 P9832", "G65 P1234 A1.", "G65 P8999", "G65 P9000",
		"M97 P5555", "M98 P1091 (angle probe)", "M98P2000", "M98 P1091",
		"T7", // a pre-stage, no M6: not a tool change
	}, "\n"))
	if info.Title != "3+1 PROBING" {
		t.Errorf("title %q", info.Title)
	}
	if want := []string{"1234", "8999", "1091", "2000", "1091"}; !reflect.DeepEqual(info.Calls, want) {
		t.Errorf("calls %v, want %v", info.Calls, want)
	}
	if want := []int{2, 3, 4}; !reflect.DeepEqual(info.Pockets, want) {
		t.Errorf("pockets %v, want %v", info.Pockets, want)
	}
}

func TestTreeFixtureO1090(t *testing.T) {
	tr := BuildProgramTreeCached(NewParseCache(), "testdata/o1090", "1090")
	if got, want := groupNumbers(tr), []string{"1090", "1091", "2000", "1092", "2001", "2002", "2003"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("groups %v, want %v", got, want)
	}
	states := map[string]string{"1090": SheetNoJSON, "1091": SheetNoJSON, "2000": SheetNoJSON, "1092": SheetNoJSON,
		"2001": SheetReady, "2002": SheetReady, "2003": SheetReady}
	for _, g := range tr.Groups {
		if g.State != states[g.Number] || g.Calls != 1 {
			t.Errorf("O%s: state %s calls %d", g.Number, g.State, g.Calls)
		}
	}
	if !reflect.DeepEqual(tr.Main().Pockets, []int{2, 3}) {
		t.Errorf("main pockets %v (T20 probe is left out)", tr.Main().Pockets)
	}
	if g := tr.Groups[1]; len(g.Pockets) != 0 || len(g.Tools()) != 0 {
		t.Errorf("O1091 should list nothing: %+v", g)
	}
	if g := tr.Groups[2]; !reflect.DeepEqual(g.Pockets, []int{2, 3}) {
		t.Errorf("O2000 pockets %v", g.Pockets)
	}
	if g := tr.Groups[4]; g.Title != "angle mill" || len(g.Tools()) != 4 {
		t.Errorf("O2001: title %q tools %d", g.Title, len(g.Tools()))
	}
	if tr.Main().Title != "3+1 PROBING" || tr.Groups[2].Title == "" {
		t.Errorf("O-line titles: main %q, O2000 %q", tr.Main().Title, tr.Groups[2].Title)
	}
	// The real job shares T2/T3/T4/T10/T14/T18 between programs with the
	// same tool each time: nothing to alert.
	if len(tr.Alerts) != 0 {
		t.Errorf("alerts on the real tree: %+v", tr.Alerts)
	}
	s := treeSheet(tr)
	if s.State != SheetReady || len(s.Groups) != 7 {
		t.Fatalf("sheet %s, %d groups", s.State, len(s.Groups))
	}
	pockets := map[int]bool{}
	for _, tl := range s.Tools {
		if pockets[tl.Pocket] {
			t.Errorf("flat tools list T%d twice", tl.Pocket)
		}
		pockets[tl.Pocket] = true
	}
	for _, p := range []int{2, 3, 4, 7, 10, 12, 13, 14, 16, 17, 18} {
		if !pockets[p] {
			t.Errorf("flat tools lack T%d", p)
		}
	}
}

func TestTreeCallOrderCountAndIgnored(t *testing.T) {
	d := newTreeDir(t)
	d.nc("100", "T2 M06", "M98 P200", "M00", "M98 P300", "M00", "M98 P200", "G65 P9995 W1.", "M97 P400", "G65 P500", "(M98 P600)")
	d.nc("200", "T3 M6")
	d.nc("300", "T4 M6", "M98 P700")
	d.nc("500", "T5 M6")
	d.nc("700", "T6 M6")
	tr := d.build("100")
	if got, want := groupNumbers(tr), []string{"100", "200", "300", "700", "500"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("groups %v, want %v", got, want)
	}
	if tr.Groups[1].Calls != 2 || tr.Groups[2].Calls != 1 {
		t.Errorf("calls: O200 %d, O300 %d", tr.Groups[1].Calls, tr.Groups[2].Calls)
	}
	if tr.Index("9995") != -1 || tr.Index("400") != -1 || tr.Index("600") != -1 {
		t.Error("G65 P9995, M97 and comments must not be calls")
	}
	if len(alertsOf(tr, AlertMissing)) != 0 {
		t.Errorf("unexpected MISSING: %+v", tr.Alerts)
	}
}

func TestTreeMissingSubprogram(t *testing.T) {
	d := newTreeDir(t)
	d.nc("100", "M98 P250")
	tr := d.build("100")
	g := tr.Groups[1]
	if g.Number != "250" || g.State != SheetMissing {
		t.Fatalf("group %+v", g)
	}
	a := alertsOf(tr, AlertMissing)
	if len(a) != 1 || !reflect.DeepEqual(a[0].Programs, []string{"O250"}) {
		t.Fatalf("alerts %+v", tr.Alerts)
	}
	s := treeSheet(tr)
	if s.Groups[1].State != SheetMissing || s.State != SheetNoJSON {
		t.Errorf("sheet %+v", s)
	}
}

func TestTreeCycleAndDepthCap(t *testing.T) {
	d := newTreeDir(t)
	d.nc("100", "M98 P200")
	d.nc("200", "T3 M6", "M98 P100", "M98 P200")
	tr := d.build("100")
	if got := groupNumbers(tr); !reflect.DeepEqual(got, []string{"100", "200"}) {
		t.Fatalf("groups %v", got)
	}

	// A chain deeper than the cap stops at maxTreeDepth.
	d = newTreeDir(t)
	for i := 0; i < 15; i++ {
		d.nc(fmt.Sprint(100+i), fmt.Sprintf("M98 P%d", 101+i))
	}
	tr = d.build("100")
	if got := len(tr.Groups); got != maxTreeDepth+1 {
		t.Fatalf("%d groups, want %d", got, maxTreeDepth+1)
	}
}

func TestTreeAlertPocketConflict(t *testing.T) {
	d := newTreeDir(t)
	d.nc("100", "M98 P2001", "M98 P2002")
	d.nc("2001", "T16 M6")
	d.side("2001", "16:drill:0.089")
	d.nc("2002", "T16 M6")
	d.side("2002", "16:flat end mill:0.25")
	tr := d.build("100")
	a := alertsOf(tr, AlertConflict)
	if len(a) != 1 || a[0].Pocket != 16 || !strings.Contains(a[0].Detail, "T16 swap between O2001 and O2002") {
		t.Fatalf("alerts %+v", tr.Alerts)
	}
	if !tr.ConflictPocket(16) || tr.ConflictPocket(3) {
		t.Error("ConflictPocket")
	}
	// A drill of another diameter is another tool too; and a fixed pocket
	// with another tool type is raised as well.
	d.side("2002", "16:drill:0.2")
	d.nc("2003", "T3 M6")
	d.side("2003", "3:flat end mill:0.25")
	d.nc("2004", "T3 M6")
	d.side("2004", "3:ball end mill:0.25")
	d.nc("100", "M98 P2001", "M98 P2002", "M98 P2003", "M98 P2004")
	tr = d.build("100")
	if c := alertsOf(tr, AlertConflict); len(c) != 2 {
		t.Fatalf("want a CONFLICT on T16 (drill diameters) and on fixed T3 (types): %+v", tr.Alerts)
	}
}

func TestTreeAlertTwoPockets(t *testing.T) {
	d := newTreeDir(t)
	d.nc("100", "M98 P2001", "M98 P2002")
	d.nc("2001", "T7 M6")
	d.side("2001", "7:flat end mill:0.5")
	d.nc("2002", "T9 M6")
	d.side("2002", "9:flat end mill:0.5")
	tr := d.build("100")
	a := alertsOf(tr, AlertTwoPockets)
	if len(a) != 1 || !strings.Contains(a[0].Detail, "T7") || !strings.Contains(a[0].Detail, "T9") {
		t.Fatalf("alerts %+v", tr.Alerts)
	}
	d.side("2002", "9:flat end mill:0.3")
	if a := alertsOf(d.build("100"), AlertTwoPockets); len(a) != 0 {
		t.Errorf("different tools: %+v", a)
	}
}

func TestTreeAlertSizeAndFaceTolerance(t *testing.T) {
	d := newTreeDir(t)
	d.nc("100", "M98 P2001", "M98 P2002")
	d.nc("2001", "T8 M6")
	d.side("2001", "8:flat end mill:0.500")
	d.nc("2002", "T8 M6")
	d.side("2002", "8:flat end mill:0.520")
	tr := d.build("100")
	a := alertsOf(tr, AlertSize)
	if len(a) != 1 || a[0].Pocket != 8 || len(alertsOf(tr, AlertConflict)) != 0 {
		t.Fatalf("beyond .012: %+v", tr.Alerts)
	}
	d.side("2002", "8:flat end mill:0.508")
	if tr := d.build("100"); len(tr.Alerts) != 0 {
		t.Errorf("within .012: %+v", tr.Alerts)
	}
	d.side("2001", "1:face mill:2.0")
	d.side("2002", "1:face mill:2.25")
	if tr := d.build("100"); len(tr.Alerts) != 0 {
		t.Errorf("face mill within 15%%: %+v", tr.Alerts)
	}
	d.side("2002", "1:face mill:2.4")
	if a := alertsOf(d.build("100"), AlertSize); len(a) != 1 {
		t.Errorf("face mill beyond 15%%: %+v", a)
	}
}

func TestTreeAlertUnverified(t *testing.T) {
	d := newTreeDir(t)
	d.nc("100", "M98 P2001", "M98 P2002")
	d.nc("2001", "T9 M6") // no sidecar
	d.nc("2002", "T9 M6")
	d.side("2002", "9:flat end mill:0.5")
	tr := d.build("100")
	a := alertsOf(tr, AlertUnverified)
	if len(a) != 1 || a[0].Pocket != 9 {
		t.Fatalf("alerts %+v", tr.Alerts)
	}
	// A fixed pocket is a standing tool: nothing unverified about it.
	d.nc("2001", "T3 M6")
	d.side("2002", "3:flat end mill:0.25")
	if a := alertsOf(d.build("100"), AlertUnverified); len(a) != 0 {
		t.Errorf("fixed pocket: %+v", a)
	}
}

func TestTreeG65P9995Ignored(t *testing.T) {
	d := newTreeDir(t)
	d.nc("100", "M06 G43 T20 D20 H20", "G65 P9995 W154.91 A20. H-2.", "G65 P9832")
	tr := d.build("100")
	if len(tr.Groups) != 1 || len(tr.Main().Pockets) != 0 || len(tr.Alerts) != 0 {
		t.Fatalf("tree %+v", tr)
	}
}

func TestTreeQRProgram(t *testing.T) {
	d := newTreeDir(t)
	d.nc("100", "M98 P2001", "M98 P2002")
	d.nc("2001", "T3 M6")
	d.nc("2002", "T3 M6")
	if got := d.build("100").QRProgram(); got != "" {
		t.Errorf("no sheet anywhere: %q", got)
	}
	doc := func(num string) {
		dir := filepath.Join(d.dir, "O"+num)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "sheet.html"), []byte("<html>"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	doc("2002")
	if got := d.build("100").QRProgram(); got != "2002" {
		t.Errorf("first group with a sheet: %q", got)
	}
	doc("2001")
	if got := d.build("100").QRProgram(); got != "2001" {
		t.Errorf("first in call order: %q", got)
	}
	doc("100")
	if got := d.build("100").QRProgram(); got != "100" {
		t.Errorf("the main's own sheet wins: %q", got)
	}
}

func TestTreeStaleOnMemberChange(t *testing.T) {
	d := newTreeDir(t)
	d.nc("100", "M98 P2001")
	d.nc("2001", "T3 M6")
	d.side("2001", "3:flat end mill:0.25")
	tr := d.build("100")
	if tr.Stale() {
		t.Fatal("fresh tree is stale")
	}
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(d.dir, "O2001.json"), later, later); err != nil {
		t.Fatal(err)
	}
	if !tr.Stale() {
		t.Error("a member sidecar's mtime changed")
	}
	tr = d.build("100")
	if err := os.Chtimes(filepath.Join(d.dir, "O2001.nc"), later, later); err != nil {
		t.Fatal(err)
	}
	if !tr.Stale() {
		t.Error("a member .nc's mtime changed")
	}
	d.nc("2002", "T4 M6")
	d.nc("100", "M98 P2001", "M98 P2002")
	tr = d.build("100")
	d.side("2002", "4:flat end mill:0.375")
	if !tr.Stale() {
		t.Error("a sidecar appeared")
	}
}

func TestParseCacheByMtime(t *testing.T) {
	d := newTreeDir(t)
	d.nc("100", "T3 M6")
	c := NewParseCache()
	p := filepath.Join(d.dir, "O100.nc")
	a, _ := c.ncInfo(p)
	b, _ := c.ncInfo(p)
	if a != b {
		t.Error("unchanged file was parsed twice")
	}
	d.nc("100", "T3 M6", "T4 M6")
	if n, _ := c.ncInfo(p); n == a || len(n.Pockets) != 2 {
		t.Errorf("changed file not re-read: %+v", n)
	}
}

// Criterion 11: one program with a sidecar and no calls gives the same
// flat tools, state and title as before the tree.
func TestTreeSheetSingleProgramMatchesBuildSetupSheet(t *testing.T) {
	cards := fixtureCards(t)
	geom := func(p int) *PocketGeom {
		l, dia := 5.0+float64(p), 0.25
		return &PocketGeom{LengthGeom: &l, DiameterGeom: &dia}
	}
	want := BuildSetupSheet("31011", cards, geom, noRec)

	dir := t.TempDir()
	for _, f := range []string{"31011.nc", "31011.json"} {
		buf, err := os.ReadFile(filepath.Join("testdata", f))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, f), buf, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	tr := BuildProgramTreeCached(NewParseCache(), dir, "31011")
	got := BuildTreeSheet(tr, geom, noRec, NoRunPos)
	if got.State != want.State || got.Program != want.Program || got.Title != want.Title {
		t.Errorf("head: %s/%s/%q vs %s/%s/%q", got.State, got.Program, got.Title, want.State, want.Program, want.Title)
	}
	if len(want.Tools) == 0 || !reflect.DeepEqual(got.Tools, want.Tools) {
		t.Errorf("flat tools differ:\n got %+v\nwant %+v", got.Tools, want.Tools)
	}
	if len(got.Alerts) != 0 || len(got.Groups) != 1 {
		t.Errorf("groups %d alerts %+v", len(got.Groups), got.Alerts)
	}
}

func TestTreeSheetNextGroupAlertsAndRunning(t *testing.T) {
	d := newTreeDir(t)
	d.nc("100", "M98 P2001", "M98 P2002")
	d.nc("2001", "T9 M6")
	d.side("2001", "9:flat end mill:0.5")
	d.nc("2002", "T11 M6")
	d.side("2002", "11:flat end mill:0.4")
	tr := d.build("100")
	l, dia := 4.0, 0.5
	geom := func(p int) *PocketGeom {
		if p == 9 {
			return &PocketGeom{LengthGeom: &l, DiameterGeom: &dia}
		}
		return nil
	}
	s := BuildTreeSheet(tr, geom, noRec, RunPos{Running: -1, Next: 2})
	if !s.Groups[2].Next || s.Groups[1].Next || s.Groups[1].Running {
		t.Errorf("flags %+v", s.Groups)
	}
	var next []SetupAlert
	for _, a := range s.Alerts {
		if a.Kind == StatusEmpty {
			next = append(next, a)
		}
	}
	if len(next) != 1 || next[0].Pocket != 11 || !reflect.DeepEqual(next[0].Programs, []string{"O2002"}) {
		t.Errorf("alerts for the group that runs next: %+v", s.Alerts)
	}
	s = BuildTreeSheet(tr, geom, noRec, RunPos{Running: 1, Next: -1})
	if !s.Groups[1].Running {
		t.Error("running flag")
	}
}
