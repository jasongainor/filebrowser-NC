package cnc

import (
	"testing"
	"time"
)

func popSlot(n int, dia, length float64) ToolTableSlot {
	s := ToolTableSlot{Slot: n}
	l := length
	s.LengthGeom = &l
	s.EffectiveLength = &l
	if dia != 0 {
		d := dia
		s.DiameterGeom = &d
		s.EffectiveDiameter = &d
	}
	return s
}

// A library that numbers tool #1 "4mm ball aluminum" — the exact trap that
// makes the by-number describeSlot mislabel the gauge in pocket 1.
func trapLibrary() *ToolLibrary {
	return NewToolLibrary(FusionLibrary{Data: []FusionTool{
		{Description: "4mm ball aluminum", Type: "ball end mill",
			Geometry:    FusionToolGeometry{DC: 0.15748, NOF: 4},
			PostProcess: FusionPostProcess{Number: 1}},
	}})
}

func reconInputs(cfg *CutConfig, prog *ProgramIntent) ReconcileInputs {
	return ReconcileInputs{
		CutConfig:     cfg,
		CutConfigName: nameOf(cfg),
		Reserved: []ReservedSpec{
			{Pocket: 1, Kind: "gauge", ExpectedDia: ptrF(0.5011), ExpectedLen: ptrF(5.119), Tol: 0.002},
			{Pocket: 20, Kind: "work_probe"},
		},
		Program: prog,
		Now:     time.Date(2026, 6, 13, 22, 18, 0, 0, time.UTC), // 4h after the read
	}
}

func nameOf(c *CutConfig) string {
	if c == nil {
		return ""
	}
	return c.Name
}

func fixtureTable() *ToolTable {
	return &ToolTable{
		ReadAt:         time.Date(2026, 6, 13, 18, 18, 0, 0, time.UTC),
		SlotsRequested: 20,
		SlotsRead:      20,
		Source:         "read",
		Slots: []ToolTableSlot{
			popSlot(1, 0.5011, 5.1193), // gauge / master
			popSlot(4, 0.3752, 4.4317), // bull-nose Ø0.375
			popSlot(20, 0.0, 5.4391),   // work probe (Ø0, length only)
		},
	}
}

func TestReconcileGaugeNotMislabelled(t *testing.T) {
	cfg := parseFixture(t)
	table := fixtureTable()
	base := BuildToolList("m", "M", "in", true, 20, 200, table, trapLibrary())

	// The bug: base describes pocket 1 by number → "4mm ball aluminum".
	if base.Pockets[0].Description != "4mm ball aluminum" {
		t.Fatalf("precondition: base desc = %q, want the buggy '4mm ball aluminum'", base.Pockets[0].Description)
	}

	out := BuildReconciledToolList(base, table, reconInputs(cfg, nil))

	p1 := out.Pockets[0]
	if p1.ReservedKind != "gauge" {
		t.Errorf("pocket 1 reserved_kind = %q, want gauge", p1.ReservedKind)
	}
	if p1.Description == "4mm ball aluminum" {
		t.Error("pocket 1 still mislabelled as '4mm ball aluminum' — the headline bug is NOT fixed")
	}
	if p1.Description != "Gauge / master tool" {
		t.Errorf("pocket 1 description = %q, want 'Gauge / master tool'", p1.Description)
	}
	if p1.Status != "ok" {
		t.Errorf("pocket 1 status = %q, want ok (measured == master)", p1.Status)
	}
	// Work probe excluded from cut-config matching.
	if out.Pockets[19].ReservedKind != "work_probe" || out.Pockets[19].Catalog != nil {
		t.Errorf("pocket 20 should be work_probe with no catalog match: %+v", out.Pockets[19])
	}
}

func TestReconcileGaugeDrift(t *testing.T) {
	cfg := parseFixture(t)
	table := fixtureTable()
	base := BuildToolList("m", "M", "in", true, 20, 200, table, trapLibrary())

	in := reconInputs(cfg, nil)
	in.Reserved[0].ExpectedDia = ptrF(0.4950) // master should be 0.495; measured 0.5011 → 0.0061 > tol
	out := BuildReconciledToolList(base, table, in)

	if out.Pockets[0].Status != "gauge_drift" {
		t.Errorf("pocket 1 status = %q, want gauge_drift", out.Pockets[0].Status)
	}
	if out.WorstSeverity != SevStop {
		t.Errorf("worst_severity = %q, want stop", out.WorstSeverity)
	}
}

func TestReconcileNoCutConfigIsNeutral(t *testing.T) {
	table := fixtureTable()
	base := BuildToolList("m", "M", "in", true, 20, 200, table, trapLibrary())

	// No cut config, no program — only the reserved registry is in play.
	in := ReconcileInputs{
		Reserved: []ReservedSpec{{Pocket: 1, Kind: "gauge", ExpectedDia: ptrF(0.5011), ExpectedLen: ptrF(5.119)}},
		Now:      time.Now(),
	}
	out := BuildReconciledToolList(base, table, in)

	for _, p := range out.Pockets {
		if p.Status == "not_in_cutconfig" {
			t.Errorf("pocket %d got not_in_cutconfig with NO config loaded — should be neutral", p.Pocket)
		}
	}
	if out.WorstSeverity == SevWarning || out.WorstSeverity == SevAction || out.WorstSeverity == SevStop {
		t.Errorf("no-config run lit up worst_severity=%q, want neutral", out.WorstSeverity)
	}
	// The gauge fix still works without a cut config (reserved registry).
	if out.Pockets[0].Description != "Gauge / master tool" {
		t.Errorf("gauge label requires no cut config; got %q", out.Pockets[0].Description)
	}
}

func TestReconcileProgramSubsetAndMissing(t *testing.T) {
	cfg := parseFixture(t)
	table := fixtureTable()
	base := BuildToolList("m", "M", "in", true, 20, 200, table, nil)

	prog := &ProgramIntent{
		Attached:        true,
		AttachedONumber: "O03001",
		RunningONumber:  "O03001",
		Tools: []ProgramToolIntent{
			{Tool: 4, ExpectedDiameter: ptrF(0.375)}, // present, in tolerance
			{Tool: 6, ExpectedDiameter: ptrF(0.25)},  // pocket 6 empty → missing
		},
	}
	out := BuildReconciledToolList(base, table, reconInputs(cfg, prog))

	if out.Program == nil || !out.Program.Attached || !out.Program.InSync {
		t.Fatalf("program meta wrong: %+v", out.Program)
	}
	if len(out.ProgramSubset) != 2 {
		t.Fatalf("program_subset len = %d, want 2 (incl missing)", len(out.ProgramSubset))
	}
	var sawMissing bool
	for _, e := range out.ProgramSubset {
		if e.ToolNumber == 6 && e.Status == "missing" {
			sawMissing = true
		}
	}
	if !sawMissing {
		t.Errorf("tool 6 should be 'missing' in the program subset: %+v", out.ProgramSubset)
	}
	if out.WorstSeverity != SevAction {
		t.Errorf("worst_severity = %q, want action (a missing tool)", out.WorstSeverity)
	}
}

func TestReconcileFreshness(t *testing.T) {
	cfg := parseFixture(t)
	table := fixtureTable()
	base := BuildToolList("m", "M", "in", true, 20, 200, table, nil)
	out := BuildReconciledToolList(base, table, reconInputs(cfg, nil))

	if out.Freshness == nil {
		t.Fatal("freshness missing")
	}
	if out.Freshness.AgeSeconds != 4*3600 {
		t.Errorf("age = %ds, want %d (4h)", out.Freshness.AgeSeconds, 4*3600)
	}
	if !out.Freshness.Complete {
		t.Error("complete should be true (SlotsRead >= SlotsRequested)")
	}
	if out.CutConfig == nil || out.CutConfig.Name != "Test Alloy" {
		t.Errorf("cutconfig name = %+v", out.CutConfig)
	}
}
