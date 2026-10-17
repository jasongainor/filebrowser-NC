package cnc

// Setup-sheet status: for each pocket a program uses, is the machine ready
// for that tool? The rules port /home/admin/cnc/reconcile_tools.py, minus
// its Fusion guid (the sidecar has none) and minus its --confirm step:
// the manifest is written by cncd alone, when the machine itself shows the
// operator touched the tool off (see SetupTracker in cncd).
//
//	ok     the machine is ready for this tool
//	SWAP   the pocket holds a different tool than the program's
//	TOUCH  right tool, but its length was never recorded or has moved
//	EMPTY  the machine shows length 0 for the pocket
//	ERROR  a fixed pocket holds the wrong tool (fixed pockets never SWAP)

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
)

const (
	StatusOK    = "ok"
	StatusSwap  = "SWAP"
	StatusTouch = "TOUCH"
	StatusEmpty = "EMPTY"
	StatusError = "ERROR"
)

const (
	// DiameterTol is how far a milling tool's machine diameter may sit from
	// the program's (inches). Face mills get FaceTolFraction of the diameter.
	DiameterTol     = 0.012
	FaceTolFraction = 0.15
	// LengthTol is how far a drill or tap's length may drift from the
	// recorded one before it needs touching off again.
	LengthTol = 0.005
	// identityDiaTol decides "same diameter" for the recorded identity.
	identityDiaTol = 0.0005
	// touchedEpsilon is the smallest length change that counts as the
	// operator having touched the tool off (float noise guard only).
	touchedEpsilon = 0.0005
)

// fixedPockets hold the standing tools (1 face mill, 2 1in, 3 1/4, 4 3/8
// bull, 5 1/2 bull, 10 1/8, 14 chamfer/spot, 19 fan, 20 probe).
var fixedPockets = map[int]bool{1: true, 2: true, 3: true, 4: true, 5: true, 10: true, 14: true, 19: true, 20: true}

// IsFixedPocket reports whether pocket holds a standing tool.
func IsFixedPocket(p int) bool { return fixedPockets[p] }

// noDiameterTypes are the tools whose diameter offset is irrelevant:
// matched by identity + length instead of diameter.
var noDiameterTypes = []string{"drill", "tap", "reamer", "spot", "center", "counter sink", "countersink", "counter bore", "counterbore", "bore bar"}

// UsesDiameter is true for milling tools (end, face, chamfer, thread mills
// ...): the tools that take a diameter offset and are matched on it.
func UsesDiameter(t CardTool) bool {
	ty := strings.ToLower(t.Type)
	for _, k := range noDiameterTypes {
		if strings.Contains(ty, k) {
			return false
		}
	}
	return true
}

func isFace(t CardTool) bool { return strings.Contains(strings.ToLower(t.Type), "face") }

// ToolIdentity is what "the same tool" means without a Fusion guid.
type ToolIdentity struct {
	Type      string  `json:"type"`
	Diameter  float64 `json:"diameter"`
	ProductID string  `json:"product_id,omitempty"`
}

// IdentityOf is the program tool's identity.
func IdentityOf(t CardTool) ToolIdentity {
	return ToolIdentity{Type: strings.ToLower(strings.TrimSpace(t.Type)), Diameter: t.Diameter, ProductID: strings.TrimSpace(t.ProductID)}
}

// Same reports whether a and b are one tool: type and diameter match, and
// productId too when both sides have one.
func (a ToolIdentity) Same(b ToolIdentity) bool {
	if !strings.EqualFold(a.Type, b.Type) || math.Abs(a.Diameter-b.Diameter) > identityDiaTol {
		return false
	}
	if a.ProductID != "" && b.ProductID != "" && a.ProductID != b.ProductID {
		return false
	}
	return true
}

// PocketGeom is what the machine currently shows for one pocket. Nil
// fields were not read.
type PocketGeom struct {
	LengthGeom   *float64
	DiameterGeom *float64
	DiameterWear *float64
}

// GeomOf lifts a tool-table slot.
func GeomOf(s ToolTableSlot) PocketGeom {
	g := PocketGeom{LengthGeom: s.LengthGeom, DiameterGeom: s.DiameterGeom, DiameterWear: s.DiameterWear}
	if g.DiameterGeom == nil && s.EffectiveDiameter != nil {
		g.DiameterGeom = s.EffectiveDiameter
		g.DiameterWear = nil
	}
	return g
}

// Length is length_geom; ok is false when it was not read.
func (g PocketGeom) Length() (float64, bool) {
	if g.LengthGeom == nil {
		return 0, false
	}
	return *g.LengthGeom, true
}

// Diameter is the effective diameter (geometry + wear).
func (g PocketGeom) Diameter() (float64, bool) {
	if g.DiameterGeom == nil {
		return 0, false
	}
	d := *g.DiameterGeom
	if g.DiameterWear != nil {
		d += *g.DiameterWear
	}
	return d, true
}

// ManifestEntry is the last tool the operator set up in a pocket.
type ManifestEntry struct {
	ToolIdentity
	Desc        string  `json:"desc,omitempty"`
	LengthGeom  float64 `json:"length_geom"`
	ConfirmedAt string  `json:"confirmed_at"`

	// Legacy keys from reconcile_tools.py, read for migration only.
	LegacyGUID string   `json:"guid,omitempty"`
	LegacyDC   *float64 `json:"dc,omitempty"`
}

// Manifest maps pocket number (as a string, like the file) to its entry.
type Manifest map[string]ManifestEntry

// ManifestFileName lives in each machine's tool-table folder, next to the
// timestamped dumps (which must skip it).
const ManifestFileName = "manifest.json"

// LoadManifest reads path. A missing file is an empty manifest. Guid-keyed
// entries from reconcile_tools.py are migrated to the identity form; the
// returned flag says the caller should save.
func LoadManifest(path string) (Manifest, bool, error) {
	m := Manifest{}
	buf, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return m, false, nil
		}
		return nil, false, err
	}
	if err := json.Unmarshal(buf, &m); err != nil {
		return nil, false, err
	}
	migrated := false
	for k, e := range m {
		if e.LegacyGUID == "" && e.LegacyDC == nil {
			continue
		}
		if e.Diameter == 0 && e.LegacyDC != nil {
			e.Diameter = *e.LegacyDC
		}
		e.LegacyGUID, e.LegacyDC = "", nil
		m[k] = e
		migrated = true
	}
	return m, migrated, nil
}

// Save writes the manifest atomically.
func (m Manifest) Save(path string) error {
	buf, err := json.MarshalIndent(m, "", " ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// PocketStatus computes one program tool's status. geom is what the
// machine shows for the pocket (nil: nothing read). rec is the manifest
// entry for the pocket (nil: never recorded).
func PocketStatus(t CardTool, geom *PocketGeom, rec *ManifestEntry) string {
	fixed := IsFixedPocket(t.Number)
	var length float64
	if geom != nil {
		length, _ = geom.Length()
	}
	if geom == nil || length == 0 {
		return StatusEmpty
	}
	if UsesDiameter(t) {
		d, ok := geom.Diameter()
		tol := DiameterTol
		if isFace(t) {
			tol = FaceTolFraction * t.Diameter
		}
		if !ok || math.Abs(d-t.Diameter) > tol {
			if fixed {
				return StatusError
			}
			return StatusSwap
		}
		return StatusOK
	}
	// Drills, taps, reamers, spot drills: trusted by recorded identity and
	// length. A standing tool in a fixed pocket needs no record.
	if fixed {
		return StatusOK
	}
	if rec == nil {
		return StatusTouch
	}
	if !rec.Same(IdentityOf(t)) {
		return StatusSwap
	}
	if math.Abs(length-rec.LengthGeom) > LengthTol {
		return StatusTouch
	}
	return StatusOK
}

// TouchedOff reports whether a drill, tap or similar shows a new non-zero
// length now than it did (before) when its pocket was first flagged: the
// operator touched it off. Milling tools need no such test: they leave the
// flagged state by matching on diameter, which PocketStatus already sees.
func TouchedOff(t CardTool, now, before PocketGeom) bool {
	if UsesDiameter(t) {
		return false
	}
	l, ok := now.Length()
	if !ok || l == 0 {
		return false
	}
	if b, ok := before.Length(); ok && math.Abs(l-b) <= touchedEpsilon {
		return false
	}
	return true
}

// RecordFor is the manifest entry written when t is touched off at length.
func RecordFor(t CardTool, length float64, at string) ManifestEntry {
	return ManifestEntry{ToolIdentity: IdentityOf(t), Desc: t.Description, LengthGeom: length, ConfirmedAt: at}
}
