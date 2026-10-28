package cnc

// The setup-sheet payload: what GET /api/displays/{id} adds under "setup".
// Everything the display draws is decided here, so the firmware stays a
// list-and-page renderer.

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// Sheet states.
const (
	SheetNone   = "none"   // no numbered program is loaded (or not known yet)
	SheetNoJSON = "nojson" // a program is loaded but no sidecar describes it
	SheetReady  = "sheet"  // loaded program with its sidecar
)

// SetupTool is one pocket of the loaded program.
type SetupTool struct {
	Pocket      int    `json:"pocket"`
	Status      string `json:"status"`
	Label       string `json:"label"` // "1/2 drill"
	Type        string `json:"type"`
	Description string `json:"description,omitempty"`
	Holder      string `json:"holder,omitempty"`
	Diameter    string `json:"diameter,omitempty"` // inches, 4 places; mills only
	// The machine's geometry offsets for the pocket, inches to 4 places.
	// Empty when not read.
	OffsetLength   string `json:"offset_length,omitempty"`
	OffsetDiameter string `json:"offset_diameter,omitempty"`
	// On a row that is not ok, what the program expects: the diameter for
	// mills (drill/tap diameters never count) and the recorded length when
	// the record is this tool. Empty when not known.
	ExpectedLength   string `json:"expected_length,omitempty"`
	ExpectedDiameter string `json:"expected_diameter,omitempty"`
	// Non-zero wear offsets, signed, 4 places; the display warns WEAR.
	WearLength   string     `json:"wear_length,omitempty"`
	WearDiameter string     `json:"wear_diameter,omitempty"`
	Shape        *ToolShape `json:"shape,omitempty"`
}

// SetupSheet is the payload. Tools are in run order, each pocket once.
type SetupSheet struct {
	State   string      `json:"state"`
	Program string      `json:"program,omitempty"`
	Title   string      `json:"title,omitempty"`
	Tools   []SetupTool `json:"tools,omitempty"`
	// QR opens the program's setup sheet; nil when it has none.
	QR *SheetQR `json:"qr,omitempty"`
	// Groups are the programs of the call tree, the loaded program first,
	// then each called program in call order. Tools above is their deduped
	// union. Alerts are what disagrees across them.
	Groups []SetupGroup `json:"groups,omitempty"`
	Alerts []SetupAlert `json:"alerts,omitempty"`
	// QRProgram is the program the QR opens ("" when none has a sheet).
	QRProgram string `json:"-"`
}

// SetupGroup is one program of the call tree.
type SetupGroup struct {
	Program string      `json:"program"`
	Title   string      `json:"title,omitempty"`
	State   string      `json:"state"` // sheet, nojson or missing
	Calls   int         `json:"calls"`
	Running bool        `json:"running,omitempty"`
	Next    bool        `json:"next,omitempty"`
	Tools   []SetupTool `json:"tools,omitempty"`
}

// SetupAlert is one thing the operator has to look at. Pocket is 0 when
// the alert is about a program, not a pocket.
type SetupAlert struct {
	Kind     string   `json:"kind"`
	Pocket   int      `json:"pocket,omitempty"`
	Programs []string `json:"programs,omitempty"`
	Detail   string   `json:"detail,omitempty"`
}

// RunPos says which group of the tree is running, and which runs next
// (stopped between calls). -1 is unknown.
type RunPos struct{ Running, Next int }

// NoRunPos knows neither.
var NoRunPos = RunPos{Running: -1, Next: -1}

// SheetQR is a QR code as rows of hex, most significant bit first, one
// bit per module, no quiet zone; len(Rows) modules square.
type SheetQR struct {
	URL  string   `json:"url"`
	Rows []string `json:"rows"`
}

// BuildSetupSheet assembles the sheet for cards. geom returns what the
// machine shows for a pocket (nil when unknown); rec returns the manifest
// entry (nil when none).
func BuildSetupSheet(program string, cards *Cards, geom func(pocket int) *PocketGeom, rec func(pocket int) *ManifestEntry) SetupSheet {
	if program == "" {
		return SetupSheet{State: SheetNone}
	}
	if cards == nil {
		return SetupSheet{State: SheetNoJSON, Program: program}
	}
	s := SetupSheet{State: SheetReady, Program: program, Title: asciiOnly(cards.Comment)}
	for _, t := range cards.ProgramTools() {
		s.Tools = append(s.Tools, buildTool(t, geom, rec))
	}
	sort.SliceStable(s.Tools, func(i, j int) bool { return s.Tools[i].Pocket < s.Tools[j].Pocket })
	return s
}

// buildTool is one sidecar tool's row, with today's per-pocket status.
func buildTool(t CardTool, geom func(pocket int) *PocketGeom, rec func(pocket int) *ManifestEntry) SetupTool {
	shape := BuildShape(t)
	st := SetupTool{
		Pocket:      t.Number,
		Status:      PocketStatus(t, geom(t.Number), rec(t.Number)),
		Label:       ToolLabel(t),
		Type:        asciiOnly(t.Type),
		Description: asciiOnly(t.Description),
		Holder:      asciiOnly(t.Holder),
		Shape:       &shape,
	}
	if UsesDiameter(t) {
		st.Diameter = fmt.Sprintf("%.4f", t.Diameter)
	}
	addOffsets(&st, geom(t.Number))
	if st.Status != StatusOK {
		if DiameterOff(t, geom(t.Number)) {
			st.ExpectedDiameter = fmt.Sprintf("%.4f", t.Diameter)
		}
		if r := rec(t.Number); r != nil && r.Same(IdentityOf(t)) {
			st.ExpectedLength = fmt.Sprintf("%.4f", r.LengthGeom)
		}
	}
	return st
}

// addOffsets fills the machine's offsets for the pocket.
func addOffsets(st *SetupTool, g *PocketGeom) {
	if g == nil {
		return
	}
	if l, ok := g.Length(); ok {
		st.OffsetLength = fmt.Sprintf("%.4f", l)
	}
	if g.DiameterGeom != nil {
		st.OffsetDiameter = fmt.Sprintf("%.4f", *g.DiameterGeom)
	}
	st.WearLength = wearText(g.LengthWear)
	st.WearDiameter = wearText(g.DiameterWear)
}

// StatusNoJSON marks a pocket read from the NC: no sidecar, so its tool is
// not known and its size is not checked.
const StatusNoJSON = "nojson"

// BuildTreeSheet assembles the sheet for a call tree: one group per
// program, the deduped union as the flat Tools, and the alerts. pos says
// which group runs, and which runs next.
func BuildTreeSheet(tree *ProgramTree, geom func(pocket int) *PocketGeom, rec func(pocket int) *ManifestEntry, pos RunPos) SetupSheet {
	main := tree.Main()
	s := SetupSheet{State: SheetNoJSON, Program: main.Number, Title: main.Title}
	hasTools := false
	for gi, g := range tree.Groups {
		sg := SetupGroup{Program: g.Number, Title: g.Title, State: g.State, Calls: g.Calls,
			Running: gi == pos.Running, Next: gi == pos.Next}
		if g.Cards != nil {
			for _, t := range g.Tools() {
				sg.Tools = append(sg.Tools, buildTool(t, geom, rec))
			}
		} else {
			for _, p := range g.Pockets {
				st := SetupTool{Pocket: p, Status: StatusNoJSON, Label: fmt.Sprintf("T%d", p)}
				addOffsets(&st, geom(p))
				sg.Tools = append(sg.Tools, st)
			}
		}
		hasTools = hasTools || len(sg.Tools) > 0
		s.Groups = append(s.Groups, sg)
	}
	if hasTools || main.Cards != nil {
		s.State = SheetReady
	}

	// The flat list: sidecar tools first (deduped by pocket + identity),
	// then NC-only pockets nobody else covers.
	have := map[int]bool{}
	var kept []CardTool
	for _, tt := range tree.UniqueTools() {
		// Two programs may name one tool differently (a vendor product id);
		// the flat list shows the pocket once when type and diameter agree.
		dup := false
		for _, k := range kept {
			if k.Number == tt.Number && strings.EqualFold(strings.TrimSpace(k.Type), strings.TrimSpace(tt.Type)) &&
				absf(k.Diameter-tt.Diameter) <= identityDiaTol {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		kept = append(kept, tt.CardTool)
		s.Tools = append(s.Tools, s.Groups[tt.Group].Tools[tt.Index])
		have[tt.Number] = true
	}
	for _, g := range s.Groups {
		for _, st := range g.Tools {
			if st.Status == StatusNoJSON && !have[st.Pocket] {
				have[st.Pocket] = true
				s.Tools = append(s.Tools, st)
			}
		}
	}
	sort.SliceStable(s.Tools, func(i, j int) bool { return s.Tools[i].Pocket < s.Tools[j].Pocket })

	s.Alerts = append(s.Alerts, tree.Alerts...)
	if pos.Next >= 0 && pos.Next < len(s.Groups) {
		g := s.Groups[pos.Next]
		for _, st := range g.Tools {
			if st.Status != StatusNoJSON && st.Status != StatusOK {
				s.Alerts = append(s.Alerts, SetupAlert{Kind: st.Status, Pocket: st.Pocket, Programs: []string{"O" + g.Program},
					Detail: fmt.Sprintf("T%d %s is %s for O%s", st.Pocket, st.Label, st.Status, g.Program)})
			}
		}
	}
	s.QRProgram = tree.QRProgram()
	return s
}

// ToolLabel is the short list name: "1/2 drill", "3 face", "3/8 chamfer".
func ToolLabel(t CardTool) string {
	short := map[string]string{"flat": "endmill", "bull": "bull", "ball": "ball", "face": "face",
		"chamfer": "chamfer", "thread": "thread", "spot": "spot", "drill": "drill", "tap": "tap"}[ShapeKind(t)]
	if short == "" {
		short = asciiOnly(t.Type)
	}
	if lower(t.Type) == "reamer" {
		short = "reamer"
	}
	return Fraction(t.Diameter) + " " + short
}

// Fraction renders inches the way a machinist reads them: the nearest
// 1/64 when within half a thou ("1/2", "1-1/4"), otherwise 3 decimals.
func Fraction(in float64) string {
	if in <= 0 {
		return "?"
	}
	n := math.Round(in * 64)
	if math.Abs(in-n/64) <= 0.0005 {
		whole, num, den := int(n)/64, int(n)%64, 64
		for num != 0 && num%2 == 0 {
			num, den = num/2, den/2
		}
		switch {
		case num == 0:
			return fmt.Sprintf("%d", whole)
		case whole == 0:
			return fmt.Sprintf("%d/%d", num, den)
		default:
			return fmt.Sprintf("%d-%d/%d", whole, num, den)
		}
	}
	s := fmt.Sprintf("%.3f", in)
	return strings.TrimRight(strings.TrimRight(s, "0"), ".")
}

// asciiOnly makes text safe for the display's GFX font: degree signs
// become "deg", curly quotes straight, everything else non-ASCII dropped.
func asciiOnly(s string) string {
	s = strings.NewReplacer("°", " deg", "“", "\"", "”", "\"", "‘", "'", "’", "'", "Ø", "dia ").Replace(s)
	var b strings.Builder
	for _, r := range s {
		if r >= 32 && r < 127 {
			b.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

func lower(s string) string  { return strings.ToLower(s) }
func has(s, sub string) bool { return strings.Contains(s, sub) }

// wearText is a non-zero wear offset as "+0.0012"; "" when zero or unread.
func wearText(w *float64) string {
	if w == nil || math.Abs(*w) < 0.00005 {
		return ""
	}
	return fmt.Sprintf("%+.4f", *w)
}
