package cnc

import (
	"os"
	"path/filepath"
	"testing"
)

func f(v float64) *float64 { return &v }

func geom(length, dia float64) *PocketGeom {
	return &PocketGeom{LengthGeom: f(length), DiameterGeom: f(dia), DiameterWear: f(0)}
}

func rec(typ string, dia float64, length float64, product string) *ManifestEntry {
	return &ManifestEntry{ToolIdentity: ToolIdentity{Type: typ, Diameter: dia, ProductID: product}, LengthGeom: length}
}

func TestPocketStatus(t *testing.T) {
	drill := CardTool{Number: 12, Type: "drill", Diameter: 0.5}
	tap := CardTool{Number: 7, Type: "tap right hand", Diameter: 0.375, ProductID: "T1"}
	endmill := CardTool{Number: 9, Type: "flat end mill", Diameter: 0.25}
	face := CardTool{Number: 1, Type: "face mill", Diameter: 3}
	fixedMill := CardTool{Number: 3, Type: "flat end mill", Diameter: 0.25}
	chamfer := CardTool{Number: 14, Type: "chamfer mill", Diameter: 0.375}
	floatChamfer := CardTool{Number: 13, Type: "chamfer mill", Diameter: 0.375}
	spotFixed := CardTool{Number: 14, Type: "spot drill", Diameter: 0.375}

	cases := []struct {
		name string
		tool CardTool
		g    *PocketGeom
		rec  *ManifestEntry
		want string
	}{
		{"drill never recorded", drill, geom(5, 0.1), nil, StatusTouch},
		{"drill recorded, same length", drill, geom(5.003, 9), rec("drill", 0.5, 5, ""), StatusOK},
		{"drill length moved over .005", drill, geom(5.0061, 0.1), rec("drill", 0.5, 5, ""), StatusTouch},
		{"drill recorded other diameter", drill, geom(5, 0.1), rec("drill", 0.4375, 5, ""), StatusSwap},
		{"drill recorded other type", drill, geom(5, 0.1), rec("tap right hand", 0.5, 5, ""), StatusSwap},
		{"drill machine diameter is ignored", drill, geom(5, 7.7), rec("drill", 0.5, 5, ""), StatusOK},
		{"tap other productId", tap, geom(4, 0), rec("tap right hand", 0.375, 4, "T2"), StatusSwap},
		{"tap record without productId matches", tap, geom(4, 0), rec("tap right hand", 0.375, 4, ""), StatusOK},
		{"length 0 is EMPTY", drill, geom(0, 0), nil, StatusEmpty},
		{"nothing read is EMPTY", drill, nil, nil, StatusEmpty},
		{"mill diameter within .012", endmill, geom(4, 0.2615), nil, StatusOK},
		{"mill diameter off", endmill, geom(4, 0.2681), nil, StatusSwap},
		{"mill uses wear", endmill, &PocketGeom{LengthGeom: f(4), DiameterGeom: f(0.24), DiameterWear: f(0.0105)}, nil, StatusOK},
		{"mill empty", endmill, geom(0, 0.25), nil, StatusEmpty},
		{"face mill 15 percent", face, geom(4, 3.3371), nil, StatusOK},
		{"face mill beyond 15 percent", face, geom(4, 3.46), nil, StatusSwap}, // fixed? no: pocket 1 is fixed -> see below
		{"fixed mill matches", fixedMill, geom(4, 0.2504), nil, StatusOK},
		{"fixed mill wrong tool is ERROR, never SWAP", fixedMill, geom(4, 0.3125), nil, StatusError},
		{"fixed chamfer ok", chamfer, geom(3.7, 0.3772), nil, StatusOK},
		{"floating chamfer matched like a mill", floatChamfer, geom(3.7, 0.3772), nil, StatusOK},
		{"floating chamfer wrong is SWAP", floatChamfer, geom(3.7, 0.5), nil, StatusSwap},
		{"fixed spot drill needs no record", spotFixed, geom(3.7, 0), nil, StatusOK},
		{"fixed pocket empty", fixedMill, geom(0, 0.25), nil, StatusEmpty},
	}
	// Pocket 1 is a fixed pocket, so the over-tolerance face mill is an ERROR.
	cases[15].want = StatusError
	cases[15].name = "fixed face mill beyond 15 percent is ERROR"
	for _, c := range cases {
		if got := PocketStatus(c.tool, c.g, c.rec); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

func TestTouchedOff(t *testing.T) {
	drill := CardTool{Number: 12, Type: "drill", Diameter: 0.5}
	endmill := CardTool{Number: 9, Type: "flat end mill", Diameter: 0.25}
	before := *geom(5, 0)
	if TouchedOff(drill, *geom(5.0002, 0), before) {
		t.Error("float noise is not a touch-off")
	}
	if !TouchedOff(drill, *geom(5.4, 0), before) {
		t.Error("a new length is a touch-off")
	}
	if TouchedOff(drill, *geom(0, 0), before) {
		t.Error("length 0 is not a touch-off")
	}
	if TouchedOff(endmill, *geom(5.4, 0.25), before) {
		t.Error("mills leave the flagged state on diameter, not TouchedOff")
	}
}

func TestManifestMigration(t *testing.T) {
	p := filepath.Join(t.TempDir(), "manifest.json")
	legacy := `{"6":{"guid":"6e970cea","desc":"3/8 Tap RH","type":"tap right hand","dc":0.375,"length_geom":4.2414,"confirmed_at":"2026-10-16T06:36:59Z"}}`
	if err := os.WriteFile(p, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	m, migrated, err := LoadManifest(p)
	if err != nil || !migrated {
		t.Fatalf("migrated=%v err=%v", migrated, err)
	}
	e := m["6"]
	if e.Type != "tap right hand" || e.Diameter != 0.375 || e.LegacyGUID != "" || e.LegacyDC != nil || e.LengthGeom != 4.2414 {
		t.Fatalf("bad migration: %+v", e)
	}
	if err := m.Save(p); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(p)
	if s := string(raw); contains(s, "guid") || contains(s, `"dc"`) {
		t.Fatalf("legacy keys survived the save: %s", s)
	}
	_, migrated, _ = LoadManifest(p)
	if migrated {
		t.Fatal("second load must not migrate again")
	}
	// A migrated tap is recognized as the same tool.
	tap := CardTool{Number: 6, Type: "tap right hand", Diameter: 0.37499999999999994}
	if got := PocketStatus(tap, geom(4.2417, 0), &e); got != StatusOK {
		t.Fatalf("migrated entry should match: %s", got)
	}
}
