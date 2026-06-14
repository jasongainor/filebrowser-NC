package cnc

// Reconciled per-machine tool-list view. Joins the latest persisted
// ToolTable dump against the operator's Fusion library and produces a
// display-agnostic JSON payload (browser, kiosk, e-paper, etc.).
//
// Carousel semantics — pocket N permanently holds tool N. Side-mount
// magazines need a separate per-tool pocket map (see
// project_filebrowser_nc_side_mount_mapping_todo.md); until then the
// pocket and tool number are the same value.
//
// The contract is consumed by:
//   - GET /api/machines/{id}/toollist
//   - GET /api/displays/{id} (embeds the same payload as data field)

import (
	"sort"
	"strconv"
	"time"
)

// ToolListPocket is one slot in the physical magazine. EVERY pocket in
// the machine's range is included — empty pockets carry ToolNumber=nil
// (omitted as null on the JSON wire).
type ToolListPocket struct {
	Pocket       int      `json:"pocket"`
	ToolNumber   *int     `json:"tool_number"`
	Description  string   `json:"description,omitempty"`
	Diameter     *float64 `json:"diameter,omitempty"`
	Length       *float64 `json:"length,omitempty"`
	Wear         *float64 `json:"wear,omitempty"` // length wear (alias of length_wear)
	DiameterWear *float64 `json:"diameter_wear,omitempty"`

	// Reconciliation overlay — set by BuildReconciledToolList, omitted by the
	// base BuildToolList so the existing wire shape is unchanged.
	Status       string      `json:"status,omitempty"`
	Reasons      []string    `json:"reasons,omitempty"`
	ReservedKind string      `json:"reserved_kind,omitempty"`
	Catalog      *CatalogRef `json:"catalog,omitempty"`
}

// ToolListLibraryEntry is one row in the reconciled library. Includes
// up to 200 slots (Haas controller-table max). Filtered to drop "all
// three fields are 0" rows — those are unassigned offsets the operator
// has never touched.
type ToolListLibraryEntry struct {
	ToolNumber   int      `json:"tool_number"`
	Pocket       int      `json:"pocket"`
	Description  string   `json:"description,omitempty"`
	Diameter     *float64 `json:"diameter,omitempty"`
	Length       *float64 `json:"length,omitempty"`
	Wear         *float64 `json:"wear,omitempty"` // length wear (alias of length_wear)
	DiameterWear *float64 `json:"diameter_wear,omitempty"`
	Offline      bool     `json:"offline"`

	Status       string      `json:"status,omitempty"`
	Reasons      []string    `json:"reasons,omitempty"`
	ReservedKind string      `json:"reserved_kind,omitempty"`
	Catalog      *CatalogRef `json:"catalog,omitempty"`
}

// ToolListMachine is the bare-bones machine identity envelope. Kept
// tiny so an e-paper firmware can parse it without pulling in the
// full settings.Machine shape.
type ToolListMachine struct {
	ID        string `json:"id"`
	Name      string `json:"name,omitempty"`
	Connected bool   `json:"connected"`
}

// ToolList is the full response shape — see /api/machines/{id}/toollist.
type ToolList struct {
	Machine            ToolListMachine        `json:"machine"`
	LastUpdated        time.Time              `json:"last_updated,omitempty"`
	LastUpdatedDisplay string                 `json:"last_updated_display,omitempty"`
	Units              string                 `json:"units"`
	Pockets            []ToolListPocket       `json:"pockets"`
	Library            []ToolListLibraryEntry `json:"library"`

	// Reconciliation additions — set by BuildReconciledToolList.
	CutConfig     *ToolListCutConfig     `json:"cutconfig,omitempty"`
	Freshness     *ToolListFreshness     `json:"freshness,omitempty"`
	Program       *ToolListProgram       `json:"program,omitempty"`
	ProgramSubset []ToolListLibraryEntry `json:"program_subset,omitempty"`
	WorstSeverity string                 `json:"worst_severity,omitempty"`
}

// BuildToolList reconciles the inputs into a ToolList. Any of the
// inputs may be nil — a fresh install with no read yet still returns
// a populated `pockets` array of pocketCount empty entries, an empty
// `library`, and a zero LastUpdated.
//
// pocketCount is the physical magazine size — comes from
// settings.Machine.EffectiveToolSlots(). librarySize is the controller
// table max (200 for Haas NGC). Both must be > 0.
func BuildToolList(
	machineID, machineName, units string,
	connected bool,
	pocketCount, librarySize int,
	table *ToolTable,
	library *ToolLibrary,
) *ToolList {
	if pocketCount <= 0 {
		pocketCount = 20
	}
	if librarySize <= 0 {
		librarySize = 200
	}
	if units == "" {
		units = "in"
	}
	out := &ToolList{
		Machine: ToolListMachine{
			ID:        machineID,
			Name:      machineName,
			Connected: connected,
		},
		Units:   units,
		Pockets: make([]ToolListPocket, 0, pocketCount),
		Library: []ToolListLibraryEntry{},
	}

	// Index the table's slots by number for fast per-pocket lookup.
	byNum := map[int]ToolTableSlot{}
	if table != nil {
		for _, s := range table.Slots {
			byNum[s.Slot] = s
		}
		out.LastUpdated = table.ReadAt
		out.LastUpdatedDisplay = table.ReadAt.UTC().Format("2006-01-02 15:04")
	}

	// Pockets pass — always pocketCount rows, in slot order. Carousel
	// semantics: pocket N's tool number == N when populated.
	for p := 1; p <= pocketCount; p++ {
		row := ToolListPocket{Pocket: p}
		if s, ok := byNum[p]; ok && !isEmptyOrOffline(s) {
			n := p
			row.ToolNumber = &n
			row.Description = describeSlot(s, library, p)
			row.Diameter = cloneNonZeroPtr(s.EffectiveDiameter)
			row.Length = cloneNonZeroPtr(s.EffectiveLength)
			row.Wear = cloneNonZeroPtr(s.LengthWear)
			row.DiameterWear = cloneNonZeroPtr(s.DiameterWear)
		}
		out.Pockets = append(out.Pockets, row)
	}

	// Library pass — up to librarySize rows. Filter the "all-zero"
	// noise so the operator's display isn't padded with 180 empty slots.
	for n := 1; n <= librarySize; n++ {
		s, hasSlot := byNum[n]
		// Skip slots the read never covered (e.g. operator set
		// slots_to_read=20 but library_size is 200).
		if !hasSlot {
			continue
		}
		entry := ToolListLibraryEntry{
			ToolNumber:   n,
			Pocket:       n, // carousel
			Description:  describeSlot(s, library, n),
			Diameter:     cloneNonZeroPtr(s.EffectiveDiameter),
			Length:       cloneNonZeroPtr(s.EffectiveLength),
			Wear:         cloneNonZeroPtr(s.LengthWear),
			DiameterWear: cloneNonZeroPtr(s.DiameterWear),
			Offline:      len(s.Errors) > 0,
		}
		// All-zero rows are the controller's default for never-touched
		// offsets. Suppress unless the row carries a description from
		// the operator's library (the operator deliberately catalogued
		// this slot even though it isn't measured yet) OR is offline
		// (errored slots are signal — operator needs to see them so
		// they know the read failed for this tool).
		if entry.Diameter == nil && entry.Length == nil && entry.Wear == nil {
			if entry.Description == "" && !entry.Offline {
				continue
			}
		}
		out.Library = append(out.Library, entry)
	}
	sort.Slice(out.Library, func(i, j int) bool {
		return out.Library[i].ToolNumber < out.Library[j].ToolNumber
	})
	return out
}

// isEmptyOrOffline collapses the two "no usable tool" signals from a
// ToolTableSlot. The pocket pass uses this so a known-empty pocket
// reports tool_number: null instead of pointing at the slot number.
func isEmptyOrOffline(s ToolTableSlot) bool {
	if s.Empty {
		return true
	}
	if len(s.Errors) > 0 {
		return true
	}
	// A slot the controller read cleanly but with every offset at 0 is
	// effectively unset — Haas's default for "no tool here."
	if s.LengthGeom == nil && s.LengthWear == nil &&
		s.DiameterGeom == nil && s.DiameterWear == nil {
		return true
	}
	return false
}

// cloneNonZeroPtr returns a copy of the pointer's value when non-nil
// AND non-zero. Zeros collapse to nil so a "we read 0.0000 because the
// offset is unset" doesn't render as a real measurement on the
// display.
func cloneNonZeroPtr(p *float64) *float64 {
	if p == nil {
		return nil
	}
	if *p == 0 {
		return nil
	}
	v := *p
	return &v
}

// describeSlot picks the best human label for a pocket. Priority:
//  1. Operator's Fusion library description (Lookup hits)
//  2. Geometry-synthesized fallback (vendor · type · D=… · N flutes)
//  3. Empty string — caller can render "—".
func describeSlot(_ ToolTableSlot, library *ToolLibrary, n int) string {
	if library == nil {
		return ""
	}
	t, ok := library.Lookup(n)
	if !ok {
		return ""
	}
	if t.Description != "" {
		return t.Description
	}
	// Fallback: synthesize from the geometry fields. Matches the
	// description shown in the magazine view.
	parts := []string{}
	if t.Vendor != "" {
		parts = append(parts, t.Vendor)
	}
	if t.Type != "" {
		parts = append(parts, t.Type)
	}
	if t.Geometry.DC > 0 {
		parts = append(parts, "D="+ftostr(t.Geometry.DC))
	}
	if t.Geometry.NOF > 0 {
		parts = append(parts, itostr(t.Geometry.NOF)+" flutes")
	}
	return joinNonEmpty(parts, " · ")
}

// Tiny formatters — avoid pulling fmt into a hot rendering path. The
// numbers fit cleanly into 4 sig figs; the library is human-curated so
// "D=0.5" is more readable than "D=0.5000".
func ftostr(v float64) string {
	// Drop trailing zeros while keeping at least one decimal digit.
	s := strconv.FormatFloat(v, 'f', 4, 64)
	for len(s) > 1 && s[len(s)-1] == '0' {
		s = s[:len(s)-1]
	}
	if len(s) > 0 && s[len(s)-1] == '.' {
		s += "0"
	}
	return s
}
func itostr(v int) string { return strconv.FormatInt(int64(v), 10) }
func joinNonEmpty(parts []string, sep string) string {
	out := ""
	for _, p := range parts {
		if p == "" {
			continue
		}
		if out != "" {
			out += sep
		}
		out += p
	}
	return out
}
