package cnc

// Ingest of a Toolpath ".cutconfig" — a ZIP of {config.json, libraries/*,
// assets/*}. Produces a FLAT catalog of every tool across the enabled
// libraries, indexed for geometry search.
//
// CRITICAL: a tool is NEVER identified by its post-process number. In a real
// cut config one library routinely has hundreds of tools all sharing one
// template number (e.g. 374 drills all #16, 1,459 endmills all #0, and three
// different tools all #1 in the aluminum library). Identity is geometry;
// the number is at most an advisory hint. See cnc/cutmatch.go for matching.

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// ToolPriorityClass marks where a catalog tool came from: an operator's
// loaded "user" library (preferred) vs a recommendation library (suggested,
// not assumed loaded).
type ToolPriorityClass string

const (
	PriorityPreferred      ToolPriorityClass = "preferred"
	PriorityRecommendation ToolPriorityClass = "recommendation"
)

// CutConfigSettings are the tolerances + limits the cut config carries.
// Geometry matching reads these instead of hardcoding constants.
type CutConfigSettings struct {
	Unit                string  `json:"unit"`
	MajorDeviationLimit float64 `json:"major_deviation_limit"`
	MinorDeviationLimit float64 `json:"minor_deviation_limit"`
	DrillMaxOversize    float64 `json:"drill_max_oversize"`
	DrillMinUndersize   float64 `json:"drill_min_undersize"` // negative in the source
	MaxToolsPerSetup    int     `json:"max_tools_per_setup"`
	AllowBullnoseAsFlat bool    `json:"allow_bullnose_as_flat"`
}

// CatalogTool is one tool flattened out of a cut config's libraries. Never
// keyed by number; NumberHint is advisory only.
type CatalogTool struct {
	LibUUID       string            `json:"lib_uuid,omitempty"`
	LibName       string            `json:"lib_name,omitempty"`
	PriorityClass ToolPriorityClass `json:"priority_class,omitempty"`
	PriorityInt   int               `json:"priority_int,omitempty"`
	NumberHint    int               `json:"number_hint,omitempty"`
	Type          string            `json:"type,omitempty"`
	Dia           float64           `json:"dia,omitempty"`
	CornerRadius  float64           `json:"corner_radius,omitempty"`
	Flutes        int               `json:"flutes,omitempty"`
	FluteLen      float64           `json:"flute_len,omitempty"`
	OAL           float64           `json:"oal,omitempty"`
	ShoulderLen   float64           `json:"shoulder_len,omitempty"`
	GaugeLen      float64           `json:"gauge_len,omitempty"`
	Holder        string            `json:"holder,omitempty"`
	Description   string            `json:"description,omitempty"`
	GUID          string            `json:"guid,omitempty"`

	// src retains the raw Fusion tool so a future role-eligibility layer can
	// decode start-values.presets without re-reading the archive. UNUSED in
	// this scope (roles are deferred).
	src FusionTool
}

// CutConfig is a parsed .cutconfig: machining settings + a flat catalog of
// every tool across the enabled libraries, plus a type-bucket index for
// geometry candidate prefiltering.
type CutConfig struct {
	Name     string            `json:"name"`
	Prefix   string            `json:"prefix,omitempty"` // role-preset marker prefix; deferred hook
	Settings CutConfigSettings `json:"settings"`
	Tools    []CatalogTool     `json:"tools"`

	byType map[string][]int // normalizedType -> indices into Tools
}

// ─── ZIP member shapes ───────────────────────────────────────────────────

type cutConfigJSON struct {
	Version            int    `json:"version"`
	Name               string `json:"name"`
	EstimatePresetUUID string `json:"estimatePresetAssetUuid"`
	MachiningSettings  struct {
		Unit                string  `json:"unit"`
		MajorDeviationLimit float64 `json:"majorDeviationLimit"`
		MinorDeviationLimit float64 `json:"minorDeviationLimit"`
		DrillMaxOversize    float64 `json:"drillMaxOversize"`
		DrillMinUndersize   float64 `json:"drillMinUndersize"`
		MaxToolsPerSetup    int     `json:"maxToolsPerSetup"`
		AllowBullnoseAsFlat bool    `json:"allowBullnoseAsFlatEndmill"`
	} `json:"machiningSettings"`
	UserLibraries           []cutConfigLibRef `json:"userLibraries"`
	RecommendationLibraries []cutConfigLibRef `json:"recommendationLibraries"`
}

type cutConfigLibRef struct {
	UUID     string `json:"uuid"`
	Priority int    `json:"priority"`
}

// cutConfigLibFile is the in-archive library shape: the standard Fusion
// library export plus uuid/name wrappers.
type cutConfigLibFile struct {
	UUID string       `json:"uuid"`
	Name string       `json:"name"`
	Data []FusionTool `json:"data"`
}

type estimatePresetAsset struct {
	Data struct {
		Material struct {
			Prefix string `json:"prefix"`
		} `json:"material"`
	} `json:"data"`
}

// ParseCutConfig reads a .cutconfig ZIP and builds the flat catalog. The
// reader is typically an *os.File (which is an io.ReaderAt); size is the
// archive length.
func ParseCutConfig(r io.ReaderAt, size int64) (*CutConfig, error) {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return nil, fmt.Errorf("cutconfig: open zip: %w", err)
	}

	files := make(map[string]*zip.File, len(zr.File))
	for _, f := range zr.File {
		files[f.Name] = f
	}

	cfgFile := files["config.json"]
	if cfgFile == nil {
		return nil, fmt.Errorf("cutconfig: config.json missing")
	}
	var cj cutConfigJSON
	if err := readZipJSON(cfgFile, &cj); err != nil {
		return nil, fmt.Errorf("cutconfig: config.json: %w", err)
	}

	out := &CutConfig{
		Name: cj.Name,
		Settings: CutConfigSettings{
			Unit:                cj.MachiningSettings.Unit,
			MajorDeviationLimit: cj.MachiningSettings.MajorDeviationLimit,
			MinorDeviationLimit: cj.MachiningSettings.MinorDeviationLimit,
			DrillMaxOversize:    cj.MachiningSettings.DrillMaxOversize,
			DrillMinUndersize:   cj.MachiningSettings.DrillMinUndersize,
			MaxToolsPerSetup:    cj.MachiningSettings.MaxToolsPerSetup,
			AllowBullnoseAsFlat: cj.MachiningSettings.AllowBullnoseAsFlat,
		},
		byType: map[string][]int{},
	}

	// Role-preset prefix (deferred hook) — best-effort from the estimate asset.
	if cj.EstimatePresetUUID != "" {
		if af := files["assets/"+cj.EstimatePresetUUID+".json"]; af != nil {
			var asset estimatePresetAsset
			if readZipJSON(af, &asset) == nil {
				out.Prefix = asset.Data.Material.Prefix
			}
		}
	}

	// Library uuid -> priority class. A user (preferred) library wins over a
	// recommendation reference to the same uuid.
	type libMeta struct {
		class    ToolPriorityClass
		priority int
	}
	libClass := map[string]libMeta{}
	var order []string
	for _, l := range cj.UserLibraries {
		if _, seen := libClass[l.UUID]; !seen {
			order = append(order, l.UUID)
		}
		libClass[l.UUID] = libMeta{PriorityPreferred, l.Priority}
	}
	for _, l := range cj.RecommendationLibraries {
		if _, seen := libClass[l.UUID]; seen {
			continue
		}
		libClass[l.UUID] = libMeta{PriorityRecommendation, l.Priority}
		order = append(order, l.UUID)
	}

	for _, uuid := range order {
		lf := files["libraries/"+uuid+".json"]
		if lf == nil {
			continue // referenced but absent — skip rather than fail
		}
		var lib cutConfigLibFile
		if err := readZipJSON(lf, &lib); err != nil {
			return nil, fmt.Errorf("cutconfig: library %s: %w", uuid, err)
		}
		meta := libClass[uuid]
		for _, t := range lib.Data {
			if t.IsHolderOnly() {
				continue
			}
			out.Tools = append(out.Tools, CatalogTool{
				LibUUID:       uuid,
				LibName:       lib.Name,
				PriorityClass: meta.class,
				PriorityInt:   meta.priority,
				NumberHint:    t.PostProcess.Number,
				Type:          t.Type,
				Dia:           t.Geometry.DC,
				CornerRadius:  t.Geometry.RE,
				Flutes:        t.Geometry.NOF,
				FluteLen:      t.Geometry.LCF,
				OAL:           t.Geometry.OAL,
				ShoulderLen:   t.Geometry.ShoulderLength,
				GaugeLen:      t.Geometry.AssemblyGaugeLength,
				Holder:        t.Holder.Description,
				Description:   t.Description,
				GUID:          t.GUID,
				src:           t,
			})
		}
	}

	for i := range out.Tools {
		nt := normalizeToolType(out.Tools[i].Type)
		out.byType[nt] = append(out.byType[nt], i)
	}

	return out, nil
}

func readZipJSON(f *zip.File, v any) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	return json.NewDecoder(rc).Decode(v)
}

// CandidatesByType returns catalog tools whose normalized type matches.
func (c *CutConfig) CandidatesByType(normType string) []CatalogTool {
	if c == nil {
		return nil
	}
	idxs := c.byType[normType]
	out := make([]CatalogTool, 0, len(idxs))
	for _, i := range idxs {
		out = append(out, c.Tools[i])
	}
	return out
}

// normalizeToolType collapses Fusion tool-type strings into a small set of
// match buckets. Order matters: "ball"/"bull" are checked before the generic
// end-mill catch so "bull nose end mill" doesn't fall through to "flat".
func normalizeToolType(t string) string {
	s := strings.ToLower(strings.TrimSpace(t))
	switch {
	case s == "":
		return "unknown"
	case strings.Contains(s, "ball"):
		return "ball"
	case strings.Contains(s, "bull"):
		return "bull"
	case strings.Contains(s, "chamfer"):
		return "chamfer"
	case strings.Contains(s, "spot"):
		return "spot"
	case strings.Contains(s, "drill"):
		return "drill"
	case strings.Contains(s, "face"):
		return "face"
	case strings.Contains(s, "slot"):
		return "slot"
	case strings.Contains(s, "thread"):
		return "thread"
	case strings.Contains(s, "flat"), strings.Contains(s, "end mill"), strings.Contains(s, "endmill"):
		return "flat"
	default:
		return s
	}
}
