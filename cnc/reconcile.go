package cnc

// BuildReconciledToolList wraps the existing BuildToolList output and enriches
// it: it corrects each tool's identity/description (reserved label → confident
// catalog match → geometry-only label → number-based fallback only when no cut
// config is loaded), tags reserved pockets, watches the gauge for drift,
// overlays the bound program's intent, and adds freshness + program metadata.
//
// Strictly additive: the base ToolList's wire shape is unchanged; this only
// fills new omitempty fields and rewrites the description value.

import (
	"fmt"
	"math"
	"time"
)

// ─── New payload shapes ──────────────────────────────────────────────────

// CatalogRef is the geometry-resolved catalog identity attached to a row.
type CatalogRef struct {
	Description  string  `json:"description,omitempty"`
	LibName      string  `json:"lib_name,omitempty"`
	Class        string  `json:"class,omitempty"` // preferred | recommendation
	Dia          float64 `json:"dia,omitempty"`
	CornerRadius float64 `json:"corner_radius,omitempty"`
	Confidence   float64 `json:"confidence"`
	Tentative    bool    `json:"tentative,omitempty"`
}

// ToolListCutConfig identifies the active cut config so a material mismatch
// (an aluminum config against a steel job) is visible on every surface.
type ToolListCutConfig struct {
	Name  string `json:"name,omitempty"`
	Stale bool   `json:"stale,omitempty"`
}

// ToolListFreshness reports when the tool table was actually read (not the
// fetch time) and whether the read was complete.
type ToolListFreshness struct {
	TableReadAt   time.Time `json:"table_read_at,omitempty"`
	TableReadUnix int64     `json:"table_read_unix,omitempty"` // for firmware age math under SNTP
	AgeSeconds    int64     `json:"age_seconds"`
	Complete      bool      `json:"complete"`
	Source        string    `json:"source,omitempty"`
}

// ToolListProgram describes the bound program and which tools it uses.
type ToolListProgram struct {
	Attached        bool   `json:"attached"`
	AttachedONumber string `json:"attached_onumber,omitempty"`
	RunningONumber  string `json:"running_onumber,omitempty"`
	InSync          bool   `json:"in_sync"`
	ExpectedTools   []int  `json:"expected_tools,omitempty"`
}

// ─── Inputs (settings-free, so toollist.go keeps its tiny import set) ─────

// ReservedSpec is a settings.ReservedTool flattened for the cnc layer.
type ReservedSpec struct {
	Pocket      int
	Kind        string
	ExpectedDia *float64
	ExpectedLen *float64
	Tol         float64
}

// ProgramToolIntent is one tool the bound program calls, with the intended
// geometry parsed from the CAM comment headers (preflight ToolUsage).
type ProgramToolIntent struct {
	Tool                 int
	ExpectedDiameter     *float64
	ExpectedCornerRadius *float64
	UsesCutterComp       bool
}

// ProgramIntent is the bound program's tool set + binding state.
type ProgramIntent struct {
	Attached        bool
	AttachedONumber string
	RunningONumber  string
	Tools           []ProgramToolIntent
}

// ReconcileInputs carries everything the reconciler needs beyond the base
// ToolList + tool table. All fields optional.
type ReconcileInputs struct {
	CutConfig      *CutConfig
	CutConfigName  string
	CutConfigStale bool
	Reserved       []ReservedSpec
	Program        *ProgramIntent
	FallbackTolIn  float64 // used when no cut config (Machine position tolerance)
	Now            time.Time
}

// ─── Severity ladder ─────────────────────────────────────────────────────

const (
	sevNone    = "" // ok / neutral
	SevInfo    = "info"
	SevWarning = "warning"
	SevAction  = "action"
	SevStop    = "stop"
)

func severityRank(s string) int {
	switch s {
	case SevStop:
		return 4
	case SevAction:
		return 3
	case SevWarning:
		return 2
	case SevInfo:
		return 1
	default:
		return 0
	}
}

// severityFor maps a status to a severity, escalating dia_mismatch under
// cutter compensation (a wrong diameter with G41/G42 cuts the wrong size).
func severityFor(status string, cutterComp bool) string {
	switch status {
	case "gauge_drift":
		return SevStop
	case "missing", "offline", "reserved_called":
		return SevAction
	case "dia_mismatch":
		if cutterComp {
			return SevAction
		}
		return SevWarning
	case "length_mismatch", "not_in_cutconfig", "recommendation_only", "stale_table":
		return SevWarning
	default:
		return SevInfo
	}
}

// BuildReconciledToolList is the additive enrichment pass over a base ToolList.
func BuildReconciledToolList(base *ToolList, table *ToolTable, in ReconcileInputs) *ToolList {
	if base == nil {
		return base
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}

	bySlot := map[int]ToolTableSlot{}
	if table != nil {
		for _, s := range table.Slots {
			bySlot[s.Slot] = s
		}
	}
	reserved := map[int]ReservedSpec{}
	for _, r := range in.Reserved {
		reserved[r.Pocket] = r
	}
	expected := map[int]ProgramToolIntent{}
	var expectedTools []int
	if in.Program != nil {
		for _, t := range in.Program.Tools {
			expected[t.Tool] = t
			expectedTools = append(expectedTools, t.Tool)
		}
	}

	worst := sevNone

	// ── Pockets (the page everyone looks at) ──
	for i := range base.Pockets {
		row := &base.Pockets[i]
		s, inTable := bySlot[row.Pocket]
		var slotPtr *ToolTableSlot
		if inTable {
			slotPtr = &s
		}
		desc, kind, status, sev, reasons, cat := in.classify(rowCtx{
			num:       row.Pocket,
			effDia:    row.Diameter, // already nonZeroOrNil of EffectiveDiameter
			effLen:    row.Length,
			slot:      slotPtr,
			baseDesc:  row.Description,
			populated: row.ToolNumber != nil,
		}, reserved, expected)
		applyOverlay(&row.Description, &row.Status, &row.Reasons, &row.ReservedKind, &row.Catalog,
			desc, status, reasons, kind, cat)
		if severityRank(sev) > severityRank(worst) {
			worst = sev
		}
	}

	// ── Library + program subset ──
	for i := range base.Library {
		e := &base.Library[i]
		s, inTable := bySlot[e.Pocket]
		var slotPtr *ToolTableSlot
		if inTable {
			slotPtr = &s
		}
		desc, kind, status, sev, reasons, cat := in.classify(rowCtx{
			num:       e.Pocket, // carousel: pocket == tool number
			effDia:    e.Diameter,
			effLen:    e.Length,
			slot:      slotPtr,
			baseDesc:  e.Description,
			populated: true,
		}, reserved, expected)
		applyOverlay(&e.Description, &e.Status, &e.Reasons, &e.ReservedKind, &e.Catalog,
			desc, status, reasons, kind, cat)
		if severityRank(sev) > severityRank(worst) {
			worst = sev
		}
	}

	// ── Cut config + stale_table ──
	if in.CutConfigName != "" || in.CutConfig != nil {
		name := in.CutConfigName
		if name == "" {
			name = in.CutConfig.Name
		}
		base.CutConfig = &ToolListCutConfig{Name: name, Stale: in.CutConfigStale}
		if in.CutConfigStale && severityRank(SevWarning) > severityRank(worst) {
			worst = SevWarning
		}
	}

	// ── Freshness ──
	if table != nil {
		age := int64(now.Sub(table.ReadAt).Seconds())
		if age < 0 {
			age = 0
		}
		base.Freshness = &ToolListFreshness{
			TableReadAt:   table.ReadAt,
			TableReadUnix: table.ReadAt.Unix(),
			AgeSeconds:    age,
			Complete:      table.SlotsRequested == 0 || table.SlotsRead >= table.SlotsRequested,
			Source:        table.Source,
		}
	}

	// ── Program + subset ──
	if in.Program != nil {
		base.Program = &ToolListProgram{
			Attached:        in.Program.Attached,
			AttachedONumber: in.Program.AttachedONumber,
			RunningONumber:  in.Program.RunningONumber,
			InSync:          in.Program.RunningONumber == "" || in.Program.RunningONumber == in.Program.AttachedONumber,
			ExpectedTools:   expectedTools,
		}
		// One enriched row per program tool (carousel: pocket == tool), in
		// program order — includes missing tools so the program page shows them.
		for _, tnum := range expectedTools {
			if tnum >= 1 && tnum <= len(base.Pockets) {
				base.ProgramSubset = append(base.ProgramSubset, pocketToLibEntry(base.Pockets[tnum-1]))
			}
		}
	}

	base.WorstSeverity = worst
	return base
}

// rowCtx is the per-row classification context.
type rowCtx struct {
	num       int
	effDia    *float64
	effLen    *float64
	slot      *ToolTableSlot
	baseDesc  string
	populated bool
}

// classify returns the corrected description, reserved kind, status, severity,
// reasons, and catalog ref for one row.
func (in ReconcileInputs) classify(ctx rowCtx, reserved map[int]ReservedSpec, expected map[int]ProgramToolIntent) (desc, kind, status, sev string, reasons []string, cat *CatalogRef) {
	desc = ctx.baseDesc
	pi, isExpected := expected[ctx.num]

	// 1. Reserved hardware — labelled honestly, excluded from cut-config match.
	if rs, ok := reserved[ctx.num]; ok {
		kind = rs.Kind
		desc = reservedLabel(rs.Kind)
		status = "ok"
		if rs.Kind == "gauge" {
			tol := rs.Tol
			if tol <= 0 {
				tol = 0.002
			}
			if rs.ExpectedDia != nil && ctx.effDia != nil && math.Abs(*ctx.effDia-*rs.ExpectedDia) > tol {
				status = "gauge_drift"
				reasons = append(reasons, fmt.Sprintf("gauge Ø%.4f off master %.4f (tol %.4f)", *ctx.effDia, *rs.ExpectedDia, tol))
			}
			if rs.ExpectedLen != nil && ctx.effLen != nil && math.Abs(*ctx.effLen-*rs.ExpectedLen) > tol {
				status = "gauge_drift"
				reasons = append(reasons, fmt.Sprintf("gauge L%.4f off master %.4f (tol %.4f)", *ctx.effLen, *rs.ExpectedLen, tol))
			}
		}
		if isExpected {
			status = "reserved_called"
			reasons = append(reasons, "program calls a reserved pocket as a cutting tool")
		}
		sev = severityFor(status, pi.UsesCutterComp)
		return
	}

	// 2. Empty / offline pocket — only a problem if the program needs it.
	if !ctx.populated {
		if isExpected {
			if ctx.slot != nil && len(ctx.slot.Errors) > 0 {
				status = "offline"
				reasons = append(reasons, "program needs this tool but the pocket read errored")
			} else {
				status = "missing"
				reasons = append(reasons, "program needs this tool but the pocket is empty")
			}
		}
		sev = severityFor(status, pi.UsesCutterComp)
		return
	}

	// 3. Populated cutting tool — resolve identity by geometry (needs a config).
	if in.CutConfig != nil && ctx.effDia != nil {
		q := MatchQuery{Dia: *ctx.effDia}
		if isExpected {
			q.CornerRadius = pi.ExpectedCornerRadius
		}
		res := in.CutConfig.ResolveTool(q)
		if res.Tool != nil {
			cat = &CatalogRef{
				Description:  res.Tool.Description,
				LibName:      res.Tool.LibName,
				Class:        string(res.Class),
				Dia:          res.Tool.Dia,
				CornerRadius: res.Tool.CornerRadius,
				Confidence:   res.Confidence,
				Tentative:    res.Tentative,
			}
			if res.Tentative {
				desc = geometryOnlyLabel(ctx.effDia)
			} else {
				desc = res.Tool.Description
			}
			if res.Status == "recommendation_only" {
				status = "recommendation_only"
				reasons = append(reasons, "matches only a recommendation library — verify it's actually loaded")
			}
		} else {
			desc = geometryOnlyLabel(ctx.effDia)
			status = "not_in_cutconfig"
			reasons = append(reasons, "not found in any enabled library of the active cut config")
		}
	}
	// when in.CutConfig == nil we leave the base (number-based) description and
	// no status — a missing config is neutral, not "every tool is suspect".

	// 4. Program diameter overlay (overrides catalog status; highest concern).
	if isExpected && pi.ExpectedDiameter != nil && ctx.effDia != nil {
		tol := in.tol()
		if math.Abs(*ctx.effDia-*pi.ExpectedDiameter) > tol {
			status = "dia_mismatch"
			reasons = append(reasons, fmt.Sprintf("program wants Ø%.4f, measured Ø%.4f (Δ%.4f)", *pi.ExpectedDiameter, *ctx.effDia, math.Abs(*ctx.effDia-*pi.ExpectedDiameter)))
		}
	}

	sev = severityFor(status, pi.UsesCutterComp)
	return
}

// tol is the diameter-mismatch tolerance: the cut config's major deviation, or
// the machine position tolerance when no config is loaded.
func (in ReconcileInputs) tol() float64 {
	if in.CutConfig != nil && in.CutConfig.Settings.MajorDeviationLimit > 0 {
		return in.CutConfig.Settings.MajorDeviationLimit
	}
	if in.FallbackTolIn > 0 {
		return in.FallbackTolIn
	}
	return 0.005
}

func applyOverlay(desc, status *string, reasons *[]string, kind *string, cat **CatalogRef,
	newDesc, newStatus string, newReasons []string, newKind string, newCat *CatalogRef) {
	if newDesc != "" {
		*desc = newDesc
	}
	*status = newStatus
	*reasons = newReasons
	*kind = newKind
	*cat = newCat
}

func reservedLabel(kind string) string {
	switch kind {
	case "gauge":
		return "Gauge / master tool"
	case "work_probe":
		return "Work probe"
	case "spot":
		return "Spot / centre tool"
	case "spare":
		return "Reserved (spare)"
	default:
		return "Reserved"
	}
}

// geometryOnlyLabel is the honest fallback when we know the diameter but not
// the catalog identity (tentative or unresolved). Reuses ftostr from toollist.go.
func geometryOnlyLabel(dia *float64) string {
	if dia == nil {
		return ""
	}
	return "Ø" + ftostr(*dia)
}

// pocketToLibEntry projects an enriched pocket row into a library-entry shape
// for the program subset (carousel: pocket == tool number).
func pocketToLibEntry(p ToolListPocket) ToolListLibraryEntry {
	tn := p.Pocket
	if p.ToolNumber != nil {
		tn = *p.ToolNumber
	}
	return ToolListLibraryEntry{
		ToolNumber:   tn,
		Pocket:       p.Pocket,
		Description:  p.Description,
		Diameter:     p.Diameter,
		Length:       p.Length,
		Wear:         p.Wear,
		DiameterWear: p.DiameterWear,
		Offline:      p.Status == "offline",
		Status:       p.Status,
		Reasons:      p.Reasons,
		ReservedKind: p.ReservedKind,
		Catalog:      p.Catalog,
	}
}
