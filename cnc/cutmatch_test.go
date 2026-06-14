package cnc

import "testing"

func ptrI(i int) *int { return &i } // ptrF lives in tooltable_diff_test.go

func newSyntheticConfig(tools []CatalogTool, s CutConfigSettings) *CutConfig {
	c := &CutConfig{Settings: s, Tools: tools, byType: map[string][]int{}}
	for i := range c.Tools {
		nt := normalizeToolType(c.Tools[i].Type)
		c.byType[nt] = append(c.byType[nt], i)
	}
	return c
}

func TestResolveToolFixture(t *testing.T) {
	cfg := parseFixture(t)

	// 4mm ball — exact diameter, distinctive — resolves confidently.
	r := cfg.ResolveTool(MatchQuery{Type: "ball end mill", Dia: 0.15748})
	if r.Tool == nil || !approxEq(r.Tool.Dia, 0.15748, 1e-4) {
		t.Fatalf("4mm ball: tool=%v", r.Tool)
	}
	if r.Status == "not_in_cutconfig" {
		t.Errorf("4mm ball should resolve, got %q", r.Status)
	}

	// Bull-nose Ø0.375 CR0.06 — corner radius disambiguates from flats.
	r = cfg.ResolveTool(MatchQuery{Type: "bull nose end mill", Dia: 0.375, CornerRadius: ptrF(0.06)})
	if r.Tool == nil || !approxEq(r.Tool.Dia, 0.375, 1e-4) || !approxEq(r.Tool.CornerRadius, 0.06, 1e-4) {
		t.Fatalf("bull Ø0.375 CR0.06: tool=%+v", r.Tool)
	}

	// No catalog tool near Ø12 → miss.
	r = cfg.ResolveTool(MatchQuery{Dia: 12.0})
	if r.Tool != nil || r.Status != "not_in_cutconfig" {
		t.Errorf("Ø12 should miss, got tool=%v status=%q", r.Tool, r.Status)
	}

	// Unknown type + a common diameter → ambiguous → tentative.
	r = cfg.ResolveTool(MatchQuery{Dia: 0.25})
	if r.Tool == nil {
		t.Fatal("Ø0.25 should match something")
	}
	if !r.Tentative {
		t.Errorf("Ø0.25 with no type hint should be tentative (confidence %.2f)", r.Confidence)
	}
}

func TestResolveToolNilConfig(t *testing.T) {
	var cfg *CutConfig
	r := cfg.ResolveTool(MatchQuery{Type: "flat end mill", Dia: 0.25})
	if r.Tool != nil || r.Status != "not_in_cutconfig" {
		t.Errorf("nil config → not_in_cutconfig, got %+v", r)
	}
}

func TestResolveToolRecommendationOnly(t *testing.T) {
	s := CutConfigSettings{MajorDeviationLimit: 0.005, MinorDeviationLimit: 0.001}
	cfg := newSyntheticConfig([]CatalogTool{
		{Type: "flat end mill", Dia: 0.25, PriorityClass: PriorityRecommendation, Description: "rec"},
	}, s)
	r := cfg.ResolveTool(MatchQuery{Type: "flat end mill", Dia: 0.25})
	if r.Tool == nil || r.Status != "recommendation_only" {
		t.Errorf("recommendation-only match: %+v (status %q)", r.Tool, r.Status)
	}
	if r.Class != PriorityRecommendation {
		t.Errorf("Class = %q, want recommendation", r.Class)
	}
}

func TestResolveToolPreferredWinsTie(t *testing.T) {
	s := CutConfigSettings{MajorDeviationLimit: 0.005, MinorDeviationLimit: 0.001}
	cfg := newSyntheticConfig([]CatalogTool{
		{Type: "flat end mill", Dia: 0.25, PriorityClass: PriorityRecommendation, Description: "rec"},
		{Type: "flat end mill", Dia: 0.25, PriorityClass: PriorityPreferred, Description: "pref"},
	}, s)
	r := cfg.ResolveTool(MatchQuery{Type: "flat end mill", Dia: 0.25})
	if r.Tool == nil || r.Tool.Description != "pref" {
		t.Errorf("preferred should win the tie, got %v", r.Tool)
	}
	if r.Status == "recommendation_only" {
		t.Errorf("a preferred match must not be recommendation_only")
	}
}

func TestResolveToolFlutesFilter(t *testing.T) {
	s := CutConfigSettings{MajorDeviationLimit: 0.005, MinorDeviationLimit: 0.001}
	cfg := newSyntheticConfig([]CatalogTool{
		{Type: "flat end mill", Dia: 0.25, Flutes: 2, PriorityClass: PriorityPreferred, Description: "2fl"},
		{Type: "flat end mill", Dia: 0.25, Flutes: 3, PriorityClass: PriorityPreferred, Description: "3fl"},
	}, s)
	r := cfg.ResolveTool(MatchQuery{Type: "flat end mill", Dia: 0.25, Flutes: ptrI(3)})
	if r.Tool == nil || r.Tool.Description != "3fl" {
		t.Errorf("flute count should select the 3-flute tool, got %v", r.Tool)
	}
}
