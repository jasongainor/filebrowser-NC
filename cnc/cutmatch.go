package cnc

// Geometry resolver: map a measured/intended tool (type + diameter, optional
// corner radius + flutes) to the best-matching CatalogTool, with a confidence.
// This REPLACES the old describe-by-number labelling — identity is geometry,
// not the post-process number.

import (
	"math"
	"sort"
)

// MatchQuery describes the tool to resolve. Dia is required (the measured
// effective diameter, or the program's intended diameter). Type may be empty
// when the controller tool table gives no type hint — then the match is by
// diameter across all types and is usually Tentative.
type MatchQuery struct {
	Type         string
	Dia          float64
	CornerRadius *float64
	Flutes       *int
}

// MatchResult is the resolver's verdict. Tool is nil on a miss. Tentative
// means the confidence is below the trust threshold, so a caller should fall
// back to a geometry-only label rather than asserting the catalog name.
type MatchResult struct {
	Tool       *CatalogTool      `json:"tool,omitempty"`
	Confidence float64           `json:"confidence"`
	Tentative  bool              `json:"tentative,omitempty"`
	Status     string            `json:"status,omitempty"` // "" hit | recommendation_only | not_in_cutconfig
	Class      ToolPriorityClass `json:"class,omitempty"`
	DiaDelta   float64           `json:"dia_delta,omitempty"`
}

// matchConfidenceThreshold: at/above, trust the catalog tool as the label;
// below, the match is Tentative.
const matchConfidenceThreshold = 0.55

// ResolveTool returns the best CatalogTool within the cut config's tolerances.
// cfg may be nil → not_in_cutconfig (the caller treats that as "no config",
// not "tool is wrong"). Tolerances come from cfg.Settings, never hardcoded.
func (c *CutConfig) ResolveTool(q MatchQuery) MatchResult {
	if c == nil || q.Dia <= 0 {
		return MatchResult{Status: "not_in_cutconfig"}
	}
	nt := normalizeToolType(q.Type)

	majorDev := c.Settings.MajorDeviationLimit
	if majorDev <= 0 {
		majorDev = 0.005
	}
	minorDev := c.Settings.MinorDeviationLimit
	if minorDev <= 0 {
		minorDev = 0.001
	}

	// Tips of chamfer/spot/drill measure off-nominal by design; widen the
	// diameter window by the drill over/undersize band for those families.
	diaWindow := majorDev
	switch nt {
	case "chamfer", "spot", "drill":
		diaWindow = max(majorDev, max(c.Settings.DrillMaxOversize, math.Abs(c.Settings.DrillMinUndersize))) + majorDev
	}

	// Candidate pool: with a type hint, the compatible type buckets; without
	// one, every tool (diameter-only match, usually tentative).
	var pool []CatalogTool
	if nt == "unknown" {
		pool = c.Tools
	} else {
		for _, bucket := range compatibleTypes(nt, c.Settings.AllowBullnoseAsFlat) {
			pool = append(pool, c.CandidatesByType(bucket)...)
		}
	}

	type cand struct {
		t        CatalogTool
		diaDelta float64
		crDelta  float64
		complete int
	}
	var cands []cand
	for _, ct := range pool {
		dd := math.Abs(q.Dia - ct.Dia)
		if dd > diaWindow {
			continue
		}
		crDelta := 0.0
		if q.CornerRadius != nil && ct.CornerRadius > 0 {
			crDelta = math.Abs(*q.CornerRadius - ct.CornerRadius)
			if crDelta > minorDev {
				continue
			}
		}
		if q.Flutes != nil && ct.Flutes > 0 && *q.Flutes != ct.Flutes {
			continue
		}
		complete := 0
		if q.CornerRadius != nil && ct.CornerRadius > 0 {
			complete++
		}
		if q.Flutes != nil && ct.Flutes > 0 {
			complete++
		}
		cands = append(cands, cand{ct, dd, crDelta, complete})
	}
	if len(cands) == 0 {
		return MatchResult{Status: "not_in_cutconfig"}
	}

	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if math.Abs(a.diaDelta-b.diaDelta) > 1e-9 {
			return a.diaDelta < b.diaDelta
		}
		if math.Abs(a.crDelta-b.crDelta) > 1e-9 {
			return a.crDelta < b.crDelta
		}
		ap := a.t.PriorityClass == PriorityPreferred
		bp := b.t.PriorityClass == PriorityPreferred
		if ap != bp {
			return ap // preferred (user) library wins over recommendation
		}
		return a.complete > b.complete
	})

	best := cands[0]
	snug := max(0.0, 1-best.diaDelta/diaWindow)
	margin := 1.0
	if len(cands) > 1 {
		margin = min(1.0, max(0.0, (cands[1].diaDelta-best.diaDelta)/diaWindow))
	}
	completeness := float64(best.complete) / 2.0
	confidence := min(1.0, max(0.0, 0.5*snug+0.3*margin+0.2*completeness))

	tool := best.t
	res := MatchResult{
		Tool:       &tool,
		Confidence: confidence,
		DiaDelta:   best.diaDelta,
		Class:      tool.PriorityClass,
		Tentative:  confidence < matchConfidenceThreshold,
	}
	if tool.PriorityClass == PriorityRecommendation {
		res.Status = "recommendation_only"
	}
	return res
}

// compatibleTypes is the set of catalog type buckets a query type may match.
// Bullnose↔flat substitution is gated on the cut config's
// allowBullnoseAsFlatEndmill; chamfer↔spot and face↔slot are always grouped.
func compatibleTypes(nt string, bullAsFlat bool) []string {
	switch nt {
	case "flat":
		if bullAsFlat {
			return []string{"flat", "bull"}
		}
		return []string{"flat"}
	case "bull":
		if bullAsFlat {
			return []string{"bull", "flat"}
		}
		return []string{"bull"}
	case "chamfer":
		return []string{"chamfer", "spot"}
	case "spot":
		return []string{"spot", "chamfer"}
	case "face":
		return []string{"face", "slot"}
	case "slot":
		return []string{"slot", "face"}
	default:
		return []string{nt}
	}
}
