package cnc

import (
	"archive/zip"
	"bytes"
	"io"
	"math"
	"testing"
)

// syntheticCutConfigBytes builds a small, NON-personal .cutconfig in memory
// that reproduces the structural cases the parser must handle: number
// collisions (three #1s with different diameters, two #0s), a holder entry to
// skip, and two libraries (a preferred user library + a recommendation one).
// Kept in-code so no real/personal cut config is ever committed.
func syntheticCutConfigBytes(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	add := func(name, content string) {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, content); err != nil {
			t.Fatal(err)
		}
	}
	add("config.json", `{
	  "version":33,"name":"Test Alloy","estimatePresetAssetUuid":"preset1",
	  "machiningSettings":{"unit":"in","majorDeviationLimit":0.005,"minorDeviationLimit":0.001,
	    "drillMaxOversize":0.004,"drillMinUndersize":-0.004,"maxToolsPerSetup":17,
	    "allowBullnoseAsFlatEndmill":true},
	  "userLibraries":[{"uuid":"ulib","priority":1}],
	  "recommendationLibraries":[{"uuid":"rlib","priority":1}]}`)
	add("assets/preset1.json", `{"uuid":"preset1","type":"estimate-preset","data":{"material":{"prefix":"TestPfx"}}}`)
	add("libraries/ulib.json", `{"uuid":"ulib","name":"User Tools","version":33,"data":[
	  {"type":"ball end mill","description":"small ball","post-process":{"number":1},"geometry":{"DC":0.15748,"NOF":4}},
	  {"type":"ball end mill","description":"half ball","post-process":{"number":1},"geometry":{"DC":0.5,"NOF":3}},
	  {"type":"face mill","description":"face 3in","post-process":{"number":1},"geometry":{"DC":3.0,"NOF":6}},
	  {"type":"flat end mill","description":"quarter flat","post-process":{"number":0},"geometry":{"DC":0.25,"NOF":3}},
	  {"type":"drill","description":"quarter drill","post-process":{"number":0},"geometry":{"DC":0.25,"NOF":2}},
	  {"type":"bull nose end mill","description":"bull 375","post-process":{"number":4},"geometry":{"DC":0.375,"RE":0.06,"NOF":3}},
	  {"type":"holder","description":"CAT40 ER25"}
	]}`)
	add("libraries/rlib.json", `{"uuid":"rlib","name":"Reco Tools","version":33,"data":[
	  {"type":"flat end mill","description":"reco flat 625","post-process":{"number":0},"geometry":{"DC":0.625,"NOF":3}},
	  {"type":"ball end mill","description":"reco ball 750","post-process":{"number":0},"geometry":{"DC":0.75,"NOF":2}}
	]}`)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func parseFixture(t *testing.T) *CutConfig {
	t.Helper()
	b := syntheticCutConfigBytes(t)
	cfg, err := ParseCutConfig(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatalf("ParseCutConfig: %v", err)
	}
	return cfg
}

const ulibUUID = "ulib"

func TestParseCutConfigSettings(t *testing.T) {
	cfg := parseFixture(t)
	if cfg.Name != "Test Alloy" {
		t.Errorf("Name = %q, want %q", cfg.Name, "Test Alloy")
	}
	if cfg.Prefix != "TestPfx" {
		t.Errorf("Prefix = %q, want TestPfx (from estimate-preset asset)", cfg.Prefix)
	}
	s := cfg.Settings
	for _, c := range []struct {
		name      string
		got, want float64
	}{
		{"MajorDeviationLimit", s.MajorDeviationLimit, 0.005},
		{"MinorDeviationLimit", s.MinorDeviationLimit, 0.001},
		{"DrillMaxOversize", s.DrillMaxOversize, 0.004},
		{"DrillMinUndersize", s.DrillMinUndersize, -0.004},
	} {
		if !approxEq(c.got, c.want, 1e-9) {
			t.Errorf("Settings.%s = %v, want %v", c.name, c.got, c.want)
		}
	}
	if s.Unit != "in" {
		t.Errorf("Unit = %q, want in", s.Unit)
	}
	if s.MaxToolsPerSetup != 17 {
		t.Errorf("MaxToolsPerSetup = %d, want 17", s.MaxToolsPerSetup)
	}
	if !s.AllowBullnoseAsFlat {
		t.Errorf("AllowBullnoseAsFlat = false, want true")
	}
}

// The whole point: tools are NOT keyed by number, so number collisions never
// drop tools. The user library has three tools numbered 1 (ball Ø0.157, ball
// Ø0.5, face Ø3.0 — utterly different) and two numbered 0 — all must survive.
func TestParseCutConfigNumberCollisionsKeepAllTools(t *testing.T) {
	cfg := parseFixture(t)

	var ulib []CatalogTool
	for _, tl := range cfg.Tools {
		if tl.LibUUID == ulibUUID {
			ulib = append(ulib, tl)
		}
	}
	if len(ulib) != 6 {
		t.Fatalf("user-library tools = %d, want 6 (7 entries − 1 holder)", len(ulib))
	}

	var num1Dias, num0 []float64
	for _, tl := range ulib {
		switch tl.NumberHint {
		case 1:
			num1Dias = append(num1Dias, tl.Dia)
		case 0:
			num0 = append(num0, tl.Dia)
		}
	}
	if len(num1Dias) != 3 {
		t.Errorf("tools numbered 1 = %d, want 3 (collision must not drop any)", len(num1Dias))
	}
	if len(num0) != 2 {
		t.Errorf("tools numbered 0 = %d, want 2 (number<=0 must NOT be skipped here)", len(num0))
	}
	for _, want := range []float64{0.15748, 0.5, 3.0} {
		if !containsApprox(num1Dias, want, 1e-4) {
			t.Errorf("missing a #1 tool with dia≈%v; got %v", want, num1Dias)
		}
	}
}

func TestParseCutConfigSkipsHoldersAndParsesAllLibraries(t *testing.T) {
	cfg := parseFixture(t)

	for _, tl := range cfg.Tools {
		if tl.Type == "holder" {
			t.Errorf("holder type leaked into catalog: %q", tl.Description)
		}
	}

	libs := map[string]bool{}
	preferred, recommendation := 0, 0
	for _, tl := range cfg.Tools {
		libs[tl.LibUUID] = true
		switch tl.PriorityClass {
		case PriorityPreferred:
			preferred++
		case PriorityRecommendation:
			recommendation++
		}
	}
	if len(libs) != 2 {
		t.Errorf("distinct libraries = %d, want 2", len(libs))
	}
	if len(cfg.Tools) != 8 {
		t.Errorf("total tools = %d, want 8 (6 user + 2 recommendation)", len(cfg.Tools))
	}
	if preferred != 6 {
		t.Errorf("preferred tools = %d, want 6", preferred)
	}
	if recommendation != 2 {
		t.Errorf("recommendation tools = %d, want 2", recommendation)
	}
}

func TestCandidatesByType(t *testing.T) {
	cfg := parseFixture(t)
	balls := cfg.CandidatesByType("ball")
	if len(balls) == 0 {
		t.Fatal("no ball candidates")
	}
	var got bool
	for _, b := range balls {
		if normalizeToolType(b.Type) != "ball" {
			t.Errorf("non-ball in ball bucket: %q", b.Type)
		}
		if approxEq(b.Dia, 0.15748, 1e-4) {
			got = true
		}
	}
	if !got {
		t.Error("Ø0.15748 ball not among ball candidates")
	}
	if len(cfg.CandidatesByType("holder")) != 0 {
		t.Error("holder bucket should be empty")
	}
}

func approxEq(a, b, eps float64) bool { return math.Abs(a-b) <= eps }

func containsApprox(xs []float64, want, eps float64) bool {
	for _, x := range xs {
		if approxEq(x, want, eps) {
			return true
		}
	}
	return false
}
