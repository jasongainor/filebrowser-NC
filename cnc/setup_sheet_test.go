package cnc

import (
	"context"
	"os"
	"strconv"
	"testing"
)

func fixtureCards(t *testing.T) *Cards {
	t.Helper()
	buf, err := os.ReadFile("testdata/31011.json")
	if err != nil {
		t.Fatal(err)
	}
	c, err := ParseCards(buf)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestParseCardsFixture(t *testing.T) {
	c := fixtureCards(t)
	tools := c.ProgramTools()
	if len(tools) != 2 || tools[0].Number != 14 || tools[1].Number != 15 {
		t.Fatalf("run order lost: %+v", tools)
	}
	if c.Comment != "4B326FE OP0A drill and predrill" {
		t.Fatalf("comment %q", c.Comment)
	}
}

func TestParseCardsMetricToInches(t *testing.T) {
	c, err := ParseCards([]byte(`{"schema":"gmw-nc-cards/1","program":{"unit":"mm"},
	 "tools":[{"number":6,"type":"drill","diameter":12.7,"fluteLength":50.8,"bodyLength":76.2,"cornerRadius":0,"taperAngle":2.0}]}`))
	if err != nil {
		t.Fatal(err)
	}
	tl := c.Tools[0]
	if abs(tl.Diameter-0.5) > 1e-9 || abs(tl.FluteLength-2) > 1e-9 || abs(tl.BodyLength-3) > 1e-9 {
		t.Fatalf("not converted: %+v", tl)
	}
	if tl.TaperAngle != 2.0 {
		t.Fatalf("angle must not be scaled: %v", tl.TaperAngle)
	}
	if _, err := ParseCards([]byte(`{"schema":"other/1"}`)); err == nil {
		t.Fatal("foreign schema accepted")
	}
}

func TestProgramToolsDedupAndSkips(t *testing.T) {
	c := &Cards{Tools: []CardTool{
		{Number: 12, Type: "drill"}, {Number: 7, Type: "tap"}, {Number: 12, Type: "drill"},
		{Number: 19, Type: "flat end mill"}, {Number: 20, Type: "probe"}, {Number: 1, Type: "face mill"},
	}}
	got := c.ProgramTools()
	if len(got) != 3 || got[0].Number != 12 || got[1].Number != 7 || got[2].Number != 1 {
		t.Fatalf("got %+v", got)
	}
}

func TestBuildSetupSheetFixture(t *testing.T) {
	c := fixtureCards(t)
	g := map[int]*PocketGeom{14: geom(3.6889, 0.3772), 15: geom(8.4096, 0.3758)}
	s := BuildSetupSheet("31011", c,
		func(p int) *PocketGeom { return g[p] },
		func(int) *ManifestEntry { return nil })
	if s.State != SheetReady || len(s.Tools) != 2 {
		t.Fatalf("%+v", s)
	}
	chamfer, drill := s.Tools[0], s.Tools[1]
	if chamfer.Status != StatusOK || chamfer.Label != "3/8 chamfer" || chamfer.Diameter != "0.3750" {
		t.Fatalf("chamfer: %+v", chamfer)
	}
	if drill.Status != StatusTouch || drill.Label != "1/2 drill" || drill.Diameter != "" {
		t.Fatalf("drill must show no diameter and need a touch-off: %+v", drill)
	}
	if drill.Description != "1/2\" Drill 135 deg" {
		t.Fatalf("description not ASCII-safe: %q", drill.Description)
	}
	if none := BuildSetupSheet("", nil, nil, nil); none.State != SheetNone {
		t.Fatal("no program must be state none")
	}
	if nj := BuildSetupSheet("31011", nil, nil, nil); nj.State != SheetNoJSON {
		t.Fatal("program without sidecar must be state nojson")
	}
}

func TestFraction(t *testing.T) {
	for in, want := range map[float64]string{
		0.5: "1/2", 0.3125: "5/16", 0.37499999999999994: "3/8", 3: "3", 1.25: "1-1/4", 0.257: "0.257", 0.0625: "1/16",
	} {
		if got := Fraction(in); got != want {
			t.Errorf("Fraction(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestReadSlotsViaAsksOnlyTheNeededVariables(t *testing.T) {
	var asked []int
	q := func(_ context.Context, code int, v *int) (*QueryResult, error) {
		if code != 600 || v == nil {
			t.Fatalf("unexpected query Q%d", code)
		}
		asked = append(asked, *v)
		if *v == 2601+14 { // pocket 15 diameter wear: the TM-2P answers empty here
			return &QueryResult{OK: true, Value: "MACRO, 2615,"}, nil
		}
		return &QueryResult{OK: true, Value: "MACRO," + itoa(*v) + ", 1.2500"}, nil
	}
	slots, err := ReadSlotsVia(context.Background(), q, []int{15, 9})
	if err != nil || len(slots) != 2 {
		t.Fatalf("%v %v", slots, err)
	}
	want := []int{2015, 2415, 2615, 2009, 2409, 2609}
	for i, w := range want {
		if asked[i] != w {
			t.Fatalf("asked %v, want %v", asked, want)
		}
	}
	if slots[0].DiameterWear != nil || slots[0].LengthGeom == nil || *slots[0].LengthGeom != 1.25 {
		t.Fatalf("slot 15: %+v", slots[0])
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

func TestSetupSheetCarriesMachineOffsets(t *testing.T) {
	cards := &Cards{Tools: []CardTool{{Number: 15, Type: "drill", Diameter: 0.421875}}}
	length, dia := 8.4096, 0.3758
	s := BuildSetupSheet("3104", cards,
		func(int) *PocketGeom { return &PocketGeom{LengthGeom: &length, DiameterGeom: &dia} },
		func(int) *ManifestEntry { return nil })
	if len(s.Tools) != 1 || s.Tools[0].OffsetLength != "8.4096" || s.Tools[0].OffsetDiameter != "0.3758" {
		t.Fatalf("%+v", s.Tools)
	}
}
