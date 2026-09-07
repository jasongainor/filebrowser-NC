package cnc

// Program identity — the Fusion post stamps a small header comment
// block into every NC file plus a JSON sidecar next to it, so the
// daemon can link a running program to a Carbon job/operation and
// check its tools against the machine's live tool table BEFORE
// sending. See docs/PROGRAM_IDENTITY.md for the full spec (header
// format, sidecar JSON schema, and the .cps snippet that generates
// both).
//
// This file is the read side only: parse the header, load the
// sidecar, verify the body hasn't drifted from what the header
// claims, and reconcile the sidecar's tool list against a ToolTable
// read from the control. cnc/preflight.go wires this in as an
// additive "identity" section — an un-annotated program (no header)
// behaves exactly as it always has.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ── Header parsing ───────────────────────────────────────────────────────

// gmwHeaderLine matches any "(GMW-...)" comment line, used to decide
// whether a line belongs to the header block at all.
var gmwHeaderLine = regexp.MustCompile(`(?i)^\(\s*GMW-`)

var (
	gmwIDRe     = regexp.MustCompile(`(?i)^\(\s*GMW-ID\s+V(\d+)\s*\)\s*$`)
	gmwJobRe    = regexp.MustCompile(`(?i)^\(\s*GMW-JOB\s+(\S+)(?:\s+(\S+))?\s*\)\s*$`)
	gmwPartRe   = regexp.MustCompile(`(?i)^\(\s*GMW-PART\s+(.+?)\s*\)\s*$`)
	gmwPostRe   = regexp.MustCompile(`(?i)^\(\s*GMW-POST\s+(\S+)(?:\s+(.+?))?\s*\)\s*$`)
	gmwPostedRe = regexp.MustCompile(`(?i)^\(\s*GMW-POSTED\s+(\S+)\s*\)\s*$`)
	gmwToolsRe  = regexp.MustCompile(`(?i)^\(\s*GMW-TOOLS\s+(\d+)\s*\)\s*$`)
	gmwShaRe    = regexp.MustCompile(`(?i)^\(\s*GMW-SHA\s+([0-9A-Fa-f]+|PENDING)\s*\)\s*$`)
)

// Identity is the parsed header block. Fields are best-effort — a
// malformed individual line just leaves its field zero rather than
// invalidating the whole block. Only the presence of a valid
// GMW-ID line on line 1 gates whether ParseIdentity reports "found"
// at all.
type Identity struct {
	SchemaVersion int
	Job           string
	Operation     string
	Part          string
	PostName      string
	PostVersion   string
	PostedAt      time.Time
	PostedAtRaw   string
	ToolCount     int

	// HeaderSHA256 is what the header claims (GMW-SHA), uppercase hex.
	HeaderSHA256 string
	// ComputedSHA256 is sha256 of the body — everything after the
	// contiguous run of GMW- lines starting at line 1 — uppercase hex.
	ComputedSHA256 string
	// SHAStatus is "match", "mismatch", or "unstamped". The post kernel
	// has no hash function (docs/PROGRAM_IDENTITY.md), so a post writes
	// GMW-SHA PENDING and the daemon's ComputedSHA256 is authoritative;
	// "unstamped" is the normal state for a freshly posted file, not a
	// fault. Only "mismatch" means the body changed after stamping.
	SHAStatus string
	// SHAMatch is true when both are non-empty and equal, case
	// insensitively.
	SHAMatch bool

	// HeaderLineCount is how many lines made up the header block.
	// Exposed mostly for tests / debugging.
	HeaderLineCount int
}

// GMW-SHA values a post may write instead of a hash. The Fusion post
// kernel cannot hash, so the stock snippet writes PENDING.
const (
	SHAPending     = "PENDING"
	SHAUnstamped   = "unstamped"
	SHAMatchStatus = "match"
	SHAMismatch    = "mismatch"
)

// ParseIdentity scans nc for a GMW header block starting at line 1.
// Returns (nil, false) when line 1 is not a valid "(GMW-ID V<n>)"
// line — that's the only thing that gates "found"; every other field
// degrades gracefully. When found, the returned Identity also carries
// the computed body sha256 and whether it matches the header's claim.
func ParseIdentity(nc []byte) (*Identity, bool) {
	lines := splitLinesKeepEnds(nc)
	if len(lines) == 0 {
		return nil, false
	}

	firstText := trimLineEnding(lines[0])
	if !gmwIDRe.MatchString(strings.TrimSpace(firstText)) {
		return nil, false
	}

	id := &Identity{}
	consumed := 0
	for _, raw := range lines {
		text := strings.TrimSpace(trimLineEnding(raw))
		if !gmwHeaderLine.MatchString(text) {
			break
		}
		consumed += len(raw)
		id.HeaderLineCount++
		parseGMWLine(id, text)
	}

	body := nc[consumed:]
	sum := sha256.Sum256(body)
	id.ComputedSHA256 = strings.ToUpper(hex.EncodeToString(sum[:]))
	switch {
	case id.HeaderSHA256 == "" || id.HeaderSHA256 == SHAPending:
		id.HeaderSHA256 = ""
		id.SHAStatus = SHAUnstamped
	case strings.EqualFold(id.HeaderSHA256, id.ComputedSHA256):
		id.SHAMatch = true
		id.SHAStatus = SHAMatchStatus
	default:
		id.SHAStatus = SHAMismatch
	}

	return id, true
}

// parseGMWLine dispatches one header line to the field it sets.
// Unrecognized GMW- lines (future fields, typos) are silently
// ignored — tolerant parsing is the whole point.
func parseGMWLine(id *Identity, text string) {
	if m := gmwIDRe.FindStringSubmatch(text); m != nil {
		if v, err := strconv.Atoi(m[1]); err == nil {
			id.SchemaVersion = v
		}
		return
	}
	if m := gmwJobRe.FindStringSubmatch(text); m != nil {
		id.Job = m[1]
		id.Operation = m[2]
		return
	}
	if m := gmwPartRe.FindStringSubmatch(text); m != nil {
		id.Part = m[1]
		return
	}
	if m := gmwShaRe.FindStringSubmatch(text); m != nil {
		id.HeaderSHA256 = strings.ToUpper(m[1])
		return
	}
	if m := gmwPostRe.FindStringSubmatch(text); m != nil {
		id.PostName = m[1]
		id.PostVersion = m[2]
		return
	}
	if m := gmwPostedRe.FindStringSubmatch(text); m != nil {
		id.PostedAtRaw = m[1]
		if t, err := time.Parse(time.RFC3339, m[1]); err == nil {
			id.PostedAt = t
		}
		return
	}
	if m := gmwToolsRe.FindStringSubmatch(text); m != nil {
		if v, err := strconv.Atoi(m[1]); err == nil {
			id.ToolCount = v
		}
		return
	}
}

// splitLinesKeepEnds splits on "\n" but keeps the terminator attached
// to each element, so summing len() over a prefix of lines gives the
// exact byte offset into the original slice. The final element may
// have no trailing newline (EOF without one) or be empty (input ended
// exactly on a newline).
func splitLinesKeepEnds(b []byte) [][]byte {
	if len(b) == 0 {
		return nil
	}
	var out [][]byte
	start := 0
	for i := 0; i < len(b); i++ {
		if b[i] == '\n' {
			out = append(out, b[start:i+1])
			start = i + 1
		}
	}
	if start < len(b) {
		out = append(out, b[start:])
	}
	return out
}

// trimLineEnding strips a trailing "\n" and/or "\r" from a raw line
// slice (as produced by splitLinesKeepEnds) for regex matching.
func trimLineEnding(b []byte) string {
	s := string(b)
	s = strings.TrimSuffix(s, "\n")
	s = strings.TrimSuffix(s, "\r")
	return s
}

// ── Sidecar ──────────────────────────────────────────────────────────────

// SidecarTool is one tool entry in the sidecar's tools[] array. See
// docs/PROGRAM_IDENTITY.md for the JSON Schema and field meanings.
type SidecarTool struct {
	TNumber           int     `json:"t_number"`
	GUID              string  `json:"guid,omitempty"`
	Description       string  `json:"description,omitempty"`
	Type              string  `json:"type,omitempty"`
	Diameter          float64 `json:"diameter,omitempty"`
	FluteLength       float64 `json:"flute_length,omitempty"`
	OverallLength     float64 `json:"overall_length,omitempty"`
	StickoutLength    float64 `json:"stickout_length,omitempty"`
	HolderID          string  `json:"holder_id,omitempty"`
	HolderDescription string  `json:"holder_description,omitempty"`
	MinZ              float64 `json:"min_z,omitempty"`
	MaxDepth          float64 `json:"max_depth,omitempty"`
	Feed              float64 `json:"feed,omitempty"`
	Speed             float64 `json:"speed,omitempty"`
}

// SidecarOperation is one entry in the sidecar's operations[] array.
type SidecarOperation struct {
	Name         string  `json:"name"`
	Tool         int     `json:"tool"`
	WCS          string  `json:"wcs,omitempty"`
	StockToLeave float64 `json:"stock_to_leave,omitempty"`
}

// Sidecar is the full <program>.gmw.json shape.
type Sidecar struct {
	SchemaVersion int                `json:"schema_version"`
	Job           string             `json:"job"`
	Operation     string             `json:"operation,omitempty"`
	Part          string             `json:"part,omitempty"`
	PostName      string             `json:"post_name,omitempty"`
	PostVersion   string             `json:"post_version,omitempty"`
	PostedAt      time.Time          `json:"posted_at"`
	ToolCount     int                `json:"tool_count,omitempty"`
	SHA256        string             `json:"sha256"`
	Tools         []SidecarTool      `json:"tools"`
	Operations    []SidecarOperation `json:"operations,omitempty"`
}

// SidecarPath returns the sidecar path for an NC file — resolved
// entirely here so callers in http/ never need to know the
// convention. Always ncPath + ".gmw.json", next to the program.
func SidecarPath(ncPath string) string {
	return ncPath + ".gmw.json"
}

// LoadSidecar reads and JSON-decodes the sidecar at path. A missing
// file returns the underlying os error (wrapped) so callers can
// distinguish "no sidecar" (os.IsNotExist) from "corrupt sidecar"
// (a json error).
func LoadSidecar(path string) (*Sidecar, error) {
	buf, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var sc Sidecar
	if err := json.Unmarshal(buf, &sc); err != nil {
		return nil, fmt.Errorf("parse sidecar %s: %w", path, err)
	}
	return &sc, nil
}

// ── Reconciliation ───────────────────────────────────────────────────────

// ToolPocketStatus classifies where (if anywhere) a sidecar tool was
// found in the machine's tool table.
type ToolPocketStatus string

const (
	// PocketMatch — the tool's expected pocket (t_number) is loaded.
	PocketMatch ToolPocketStatus = "match"
	// PocketMoved — the expected pocket is empty/missing, but another
	// pocket carries a tool with a matching diameter.
	PocketMoved ToolPocketStatus = "moved"
	// PocketMissing — no pocket, expected or otherwise, has a
	// matching tool. Swap needed before send.
	PocketMissing ToolPocketStatus = "missing"
)

// DefaultClearanceMargin is added to a tool's max_depth to get the
// required reach, when the caller doesn't override it. Generous
// enough to cover fixture lips and touch-off error without firing on
// every ordinary job.
const DefaultClearanceMargin = 0.100

// ReconcileConfig tunes ReconcileTools. Zero values fall back to
// package defaults.
type ReconcileConfig struct {
	// ClearanceMargin is added to a tool's sidecar max_depth to get
	// the required reach. <= 0 uses DefaultClearanceMargin.
	ClearanceMargin float64
	// DiameterTolerance is how far a pocket's actual diameter may
	// drift from the sidecar's expected diameter before it's
	// considered a different tool (for pocket matching) or a warning
	// (for an already-matched pocket). <= 0 uses the package
	// DiameterTolerance (shared with preflight.go).
	DiameterTolerance float64
}

func (c ReconcileConfig) resolved() ReconcileConfig {
	if c.ClearanceMargin <= 0 {
		c.ClearanceMargin = DefaultClearanceMargin
	}
	if c.DiameterTolerance <= 0 {
		c.DiameterTolerance = DiameterTolerance
	}
	return c
}

// ToolReconcileResult is one sidecar tool's reconciliation outcome.
type ToolReconcileResult struct {
	TNumber     int    `json:"t_number"`
	GUID        string `json:"guid,omitempty"`
	Description string `json:"description,omitempty"`

	PocketStatus ToolPocketStatus `json:"pocket_status"`
	PocketReason string           `json:"pocket_reason,omitempty"`
	// ActualPocket is set only when PocketStatus is "moved" — the
	// pocket number where a matching-diameter tool was actually found.
	ActualPocket *int `json:"actual_pocket,omitempty"`

	ExpectedDiameter float64  `json:"expected_diameter,omitempty"`
	ActualDiameter   *float64 `json:"actual_diameter,omitempty"`
	DiameterWarning  bool     `json:"diameter_warning,omitempty"`
	DiameterReason   string   `json:"diameter_reason,omitempty"`

	RequiredReach       float64  `json:"required_reach,omitempty"`
	FluteLength         float64  `json:"flute_length,omitempty"`
	StickoutLength      float64  `json:"stickout_length,omitempty"`
	MachineLengthOffset *float64 `json:"machine_length_offset,omitempty"`
	LengthInsufficient  bool     `json:"length_insufficient,omitempty"`
	LengthReason        string   `json:"length_reason,omitempty"`
}

// ReconcileSummary counts each outcome across a ReconcileTools run.
type ReconcileSummary struct {
	Match              int `json:"match"`
	Moved              int `json:"moved"`
	Missing            int `json:"missing"`
	DiameterWarning    int `json:"diameter_warning"`
	LengthInsufficient int `json:"length_insufficient"`
}

// ToolReconcileReport is the result of ReconcileTools.
type ToolReconcileReport struct {
	Tools   []ToolReconcileResult `json:"tools"`
	Summary ReconcileSummary      `json:"summary"`
}

// ReconcileTools checks every tool the sidecar describes against the
// machine's tool table. table may be nil (no read on file yet) — every
// tool then reports PocketMissing since nothing can be confirmed
// loaded. See docs/PROGRAM_IDENTITY.md section 4 for the algorithm in
// prose.
func ReconcileTools(sidecar *Sidecar, table *ToolTable, cfg ReconcileConfig) *ToolReconcileReport {
	cfg = cfg.resolved()
	report := &ToolReconcileReport{Tools: []ToolReconcileResult{}}
	if sidecar == nil {
		return report
	}

	slots := map[int]*ToolTableSlot{}
	if table != nil {
		for i := range table.Slots {
			s := &table.Slots[i]
			slots[s.Slot] = s
		}
	}

	for _, st := range sidecar.Tools {
		res := ToolReconcileResult{
			TNumber:          st.TNumber,
			GUID:             st.GUID,
			Description:      st.Description,
			ExpectedDiameter: st.Diameter,
			FluteLength:      st.FluteLength,
			StickoutLength:   st.StickoutLength,
			RequiredReach:    st.MaxDepth + cfg.ClearanceMargin,
		}

		var pocketSlot *ToolTableSlot
		if expected, ok := slots[st.TNumber]; ok && slotLoaded(expected) {
			res.PocketStatus = PocketMatch
			pocketSlot = expected
		} else if moved, num, ok := findByDiameter(slots, st.TNumber, st.Diameter, cfg.DiameterTolerance); ok {
			res.PocketStatus = PocketMoved
			res.ActualPocket = &num
			pocketSlot = moved
		} else {
			res.PocketStatus = PocketMissing
			res.PocketReason = fmt.Sprintf(
				"no loaded tool matching T%d (⌀%.4f expected) found in the tool table — swap needed",
				st.TNumber, st.Diameter,
			)
		}

		if pocketSlot != nil {
			if dia := slotDiameter(pocketSlot); dia != nil {
				res.ActualDiameter = dia
				if delta := *dia - st.Diameter; abs(delta) > cfg.DiameterTolerance {
					res.DiameterWarning = true
					res.DiameterReason = fmt.Sprintf(
						"expected ⌀%.4f, table reports ⌀%.4f (Δ %+.4f)",
						st.Diameter, *dia, delta,
					)
				}
			}
			if length := slotLength(pocketSlot); length != nil {
				res.MachineLengthOffset = length
			}
		}

		reasons := make([]string, 0, 2)
		if st.FluteLength > 0 && res.RequiredReach > st.FluteLength {
			reasons = append(reasons, fmt.Sprintf(
				"required reach %.4f exceeds flute length %.4f", res.RequiredReach, st.FluteLength,
			))
		}
		if st.StickoutLength > 0 && res.RequiredReach > st.StickoutLength {
			reasons = append(reasons, fmt.Sprintf(
				"required reach %.4f exceeds stickout length %.4f", res.RequiredReach, st.StickoutLength,
			))
		}
		if res.MachineLengthOffset != nil && *res.MachineLengthOffset < res.RequiredReach {
			reasons = append(reasons, fmt.Sprintf(
				"pocket T%d geometry length offset %.4f is less than required reach %.4f",
				st.TNumber, *res.MachineLengthOffset, res.RequiredReach,
			))
		}
		if len(reasons) > 0 {
			res.LengthInsufficient = true
			res.LengthReason = strings.Join(reasons, "; ")
		}

		switch res.PocketStatus {
		case PocketMatch:
			report.Summary.Match++
		case PocketMoved:
			report.Summary.Moved++
		case PocketMissing:
			report.Summary.Missing++
		}
		if res.DiameterWarning {
			report.Summary.DiameterWarning++
		}
		if res.LengthInsufficient {
			report.Summary.LengthInsufficient++
		}

		report.Tools = append(report.Tools, res)
	}

	return report
}

// slotLoaded mirrors preflight's "loaded" classification: read OK
// (no per-field errors) and not an empty pocket.
func slotLoaded(s *ToolTableSlot) bool {
	if s == nil {
		return false
	}
	if hasErrors(s) {
		return false
	}
	if s.Empty {
		return false
	}
	return slotDiameter(s) != nil || slotLength(s) != nil
}

// findByDiameter searches every populated, non-errored slot OTHER
// than `exclude` for one whose diameter is within tol of want. Returns
// the slot, its number, and true on the first (lowest-numbered) match
// — deterministic given map iteration order isn't.
func findByDiameter(slots map[int]*ToolTableSlot, exclude int, want, tol float64) (*ToolTableSlot, int, bool) {
	var bestNum int
	var best *ToolTableSlot
	for n, s := range slots {
		if n == exclude {
			continue
		}
		if !slotLoaded(s) {
			continue
		}
		dia := slotDiameter(s)
		if dia == nil || abs(*dia-want) > tol {
			continue
		}
		if best == nil || n < bestNum {
			best, bestNum = s, n
		}
	}
	if best == nil {
		return nil, 0, false
	}
	return best, bestNum, true
}

// slotDiameter prefers the pre-computed EffectiveDiameter and falls
// back to the raw geometry base when wear hasn't been read.
func slotDiameter(s *ToolTableSlot) *float64 {
	if s.EffectiveDiameter != nil {
		return s.EffectiveDiameter
	}
	return s.DiameterGeom
}

// slotLength prefers EffectiveLength, falling back to the raw
// geometry base.
func slotLength(s *ToolTableSlot) *float64 {
	if s.EffectiveLength != nil {
		return s.EffectiveLength
	}
	return s.LengthGeom
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

// ── Preflight integration ────────────────────────────────────────────────

// IdentityReport is the "identity" section BuildPreflight attaches to
// a Preflight result when the NC file carries a GMW header. nil when
// the header is absent — preflight then behaves exactly as it always
// has (T-number-only, via Preflight.Tools).
type IdentityReport struct {
	SchemaVersion int    `json:"schema_version,omitempty"`
	Job           string `json:"job,omitempty"`
	Operation     string `json:"operation,omitempty"`
	Part          string `json:"part,omitempty"`
	PostName      string `json:"post_name,omitempty"`
	PostVersion   string `json:"post_version,omitempty"`
	PostedAt      string `json:"posted_at,omitempty"`

	HeaderSHA256   string `json:"header_sha256,omitempty"`
	ComputedSHA256 string `json:"computed_sha256,omitempty"`
	SHAMatch       bool   `json:"sha_match"`
	SHAStatus      string `json:"sha_status"`

	SidecarFound bool   `json:"sidecar_found"`
	SidecarError string `json:"sidecar_error,omitempty"`

	Tools   []ToolReconcileResult `json:"tools,omitempty"`
	Summary ReconcileSummary      `json:"summary,omitempty"`
}

// BuildIdentityReport parses nc for a GMW header and, when found,
// loads and reconciles the sidecar at SidecarPath(absPath) against
// table. Returns nil when nc carries no header — the caller (
// BuildPreflight) leaves Preflight.Identity unset in that case so an
// un-annotated program's report is byte-for-byte what it always was.
// table may be nil.
func BuildIdentityReport(absPath string, nc []byte, table *ToolTable) *IdentityReport {
	id, ok := ParseIdentity(nc)
	if !ok {
		return nil
	}

	rep := &IdentityReport{
		SchemaVersion:  id.SchemaVersion,
		Job:            id.Job,
		Operation:      id.Operation,
		Part:           id.Part,
		PostName:       id.PostName,
		PostVersion:    id.PostVersion,
		HeaderSHA256:   id.HeaderSHA256,
		ComputedSHA256: id.ComputedSHA256,
		SHAMatch:       id.SHAMatch,
		SHAStatus:      id.SHAStatus,
	}
	if !id.PostedAt.IsZero() {
		rep.PostedAt = id.PostedAt.Format(time.RFC3339)
	}

	sidecar, err := LoadSidecar(SidecarPath(absPath))
	if err != nil {
		rep.SidecarError = err.Error()
		return rep
	}
	rep.SidecarFound = true

	result := ReconcileTools(sidecar, table, ReconcileConfig{})
	rep.Tools = result.Tools
	rep.Summary = result.Summary
	return rep
}
