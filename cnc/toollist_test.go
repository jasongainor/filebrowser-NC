package cnc

import (
	"encoding/json"
	"testing"
	"time"
)

func TestBuildToolListPocketsAlwaysFullCount(t *testing.T) {
	out := BuildToolList("m1", "Mill 1", "in", true, 20, 200, nil, nil)
	if len(out.Pockets) != 20 {
		t.Fatalf("expected 20 pockets, got %d", len(out.Pockets))
	}
	for i, p := range out.Pockets {
		if p.Pocket != i+1 {
			t.Fatalf("pocket %d has wrong slot %d", i+1, p.Pocket)
		}
		if p.ToolNumber != nil {
			t.Fatalf("empty pocket %d should have nil tool_number, got %d", i+1, *p.ToolNumber)
		}
	}
	if len(out.Library) != 0 {
		t.Fatalf("empty library should be empty, got %d", len(out.Library))
	}
}

func TestBuildToolListPocketCarouselMapping(t *testing.T) {
	d := 0.5
	l := 3.25
	w := 0.001
	tbl := &ToolTable{
		MachineID: "m1",
		ReadAt:    time.Date(2026, 6, 3, 14, 22, 10, 0, time.UTC),
		Slots: []ToolTableSlot{
			{Slot: 1, EffectiveDiameter: &d, EffectiveLength: &l, LengthWear: &w, LengthGeom: &l, DiameterGeom: &d},
			{Slot: 2, Empty: true},
			{Slot: 3, Errors: map[string]string{"length_geom": "timeout"}},
		},
	}
	out := BuildToolList("m1", "Mill 1", "in", true, 20, 200, tbl, nil)
	if out.Pockets[0].ToolNumber == nil || *out.Pockets[0].ToolNumber != 1 {
		t.Fatalf("pocket 1 should be tool 1, got %+v", out.Pockets[0])
	}
	if out.Pockets[0].Diameter == nil || *out.Pockets[0].Diameter != 0.5 {
		t.Fatalf("pocket 1 diameter wrong: %+v", out.Pockets[0])
	}
	if out.Pockets[1].ToolNumber != nil {
		t.Fatalf("empty pocket 2 should be null, got %d", *out.Pockets[1].ToolNumber)
	}
	if out.Pockets[2].ToolNumber != nil {
		t.Fatalf("errored pocket 3 should be null, got %d", *out.Pockets[2].ToolNumber)
	}
	if out.LastUpdated.IsZero() {
		t.Fatal("last_updated should be set from table")
	}
	if out.LastUpdatedDisplay != "2026-06-03 14:22" {
		t.Fatalf("display ts wrong: %q", out.LastUpdatedDisplay)
	}
}

func TestBuildToolListLibraryDropsAllZeroRows(t *testing.T) {
	d := 0.5
	l := 3.0
	zero := 0.0
	tbl := &ToolTable{
		Slots: []ToolTableSlot{
			// real tool — keep
			{Slot: 1, LengthGeom: &l, DiameterGeom: &d, EffectiveDiameter: &d, EffectiveLength: &l},
			// every offset is 0 → controller default → drop
			{Slot: 2, LengthGeom: &zero, LengthWear: &zero, DiameterGeom: &zero, DiameterWear: &zero},
			// errored slot — keep with offline=true
			{Slot: 5, Errors: map[string]string{"length_geom": "timeout"}},
		},
	}
	out := BuildToolList("m1", "Mill 1", "in", true, 20, 200, tbl, nil)
	// Library should include slot 1 + slot 5; slot 2 dropped as
	// "all-zero, no description"
	if len(out.Library) != 2 {
		t.Fatalf("expected 2 library entries, got %d (%+v)", len(out.Library), out.Library)
	}
	if out.Library[0].ToolNumber != 1 {
		t.Fatalf("first library entry should be tool 1, got %d", out.Library[0].ToolNumber)
	}
	if out.Library[1].ToolNumber != 5 || !out.Library[1].Offline {
		t.Fatalf("second library entry should be tool 5 offline, got %+v", out.Library[1])
	}
}

func TestBuildToolListLibraryKeepsZeroSlotWithDescription(t *testing.T) {
	zero := 0.0
	tbl := &ToolTable{
		Slots: []ToolTableSlot{
			{Slot: 4, LengthGeom: &zero, LengthWear: &zero,
				DiameterGeom: &zero, DiameterWear: &zero},
		},
	}
	// Library entry for slot 4 — operator catalogued it but it's not
	// measured yet.
	lib := NewToolLibrary(FusionLibrary{
		Data: []FusionTool{
			{
				Description: "Helical 1/4 chamfer",
				PostProcess: FusionPostProcess{Number: 4},
				Geometry:    FusionToolGeometry{DC: 0.25},
			},
		},
	})
	out := BuildToolList("m1", "Mill 1", "in", true, 20, 200, tbl, lib)
	if len(out.Library) != 1 {
		t.Fatalf("expected the zero-row to survive with desc, got %d", len(out.Library))
	}
	if out.Library[0].Description != "Helical 1/4 chamfer" {
		t.Fatalf("library description not pulled from Fusion: %+v", out.Library[0])
	}
}

func TestBuildToolListJSONShapeMatchesContract(t *testing.T) {
	d := 0.5
	l := 3.5
	tbl := &ToolTable{
		ReadAt: time.Date(2026, 6, 3, 14, 22, 10, 0, time.UTC),
		Slots: []ToolTableSlot{
			{Slot: 1, EffectiveDiameter: &d, EffectiveLength: &l,
				LengthGeom: &l, DiameterGeom: &d},
		},
	}
	out := BuildToolList("vf2", "Haas VF-2", "in", true, 20, 200, tbl, nil)
	body, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	for _, want := range []string{
		`"machine":{"id":"vf2"`,
		`"connected":true`,
		`"last_updated":"2026-06-03T14:22:10Z"`,
		`"last_updated_display":"2026-06-03 14:22"`,
		`"units":"in"`,
		`"pockets":[`,
		`"library":[`,
	} {
		if !contains(s, want) {
			t.Fatalf("missing %q in JSON: %s", want, s)
		}
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
