package cnc

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ── ParseIdentity ────────────────────────────────────────────────────────

func TestParseIdentityWellFormed(t *testing.T) {
	body := "N10 G20\nN20 G90 G54\nN30 T5 M06\n"
	sum := sha256.Sum256([]byte(body))
	sha := strings.ToUpper(hex.EncodeToString(sum[:]))

	header := "(GMW-ID V1)\n" +
		"(GMW-JOB J000020 OP10)\n" +
		"(GMW-PART F-BRACKET-REV-C)\n" +
		"(GMW-POST HAAS-NGC V1.0.3)\n" +
		"(GMW-POSTED 2026-09-07T14:22:00Z)\n" +
		"(GMW-TOOLS 1)\n" +
		"(GMW-SHA " + sha + ")\n"

	nc := []byte(header + body)
	id, ok := ParseIdentity(nc)
	if !ok {
		t.Fatalf("expected identity found")
	}
	if id.SchemaVersion != 1 {
		t.Errorf("schema version = %d, want 1", id.SchemaVersion)
	}
	if id.Job != "J000020" || id.Operation != "OP10" {
		t.Errorf("job/op = %q/%q, want J000020/OP10", id.Job, id.Operation)
	}
	if id.Part != "F-BRACKET-REV-C" {
		t.Errorf("part = %q", id.Part)
	}
	if id.PostName != "HAAS-NGC" || id.PostVersion != "V1.0.3" {
		t.Errorf("post = %q/%q", id.PostName, id.PostVersion)
	}
	if id.PostedAt.IsZero() {
		t.Errorf("posted_at not parsed")
	}
	if id.ToolCount != 1 {
		t.Errorf("tool count = %d, want 1", id.ToolCount)
	}
	if !id.SHAMatch {
		t.Errorf("expected sha match, header=%s computed=%s", id.HeaderSHA256, id.ComputedSHA256)
	}
	if id.HeaderLineCount != 7 {
		t.Errorf("header line count = %d, want 7", id.HeaderLineCount)
	}
}

func TestParseIdentityAbsent(t *testing.T) {
	nc := []byte("N10 G20\nN20 G90 G54\nT5 M06\n")
	id, ok := ParseIdentity(nc)
	if ok || id != nil {
		t.Fatalf("expected no identity, got %+v ok=%v", id, ok)
	}
}

func TestParseIdentityEmptyFile(t *testing.T) {
	id, ok := ParseIdentity(nil)
	if ok || id != nil {
		t.Fatalf("expected no identity on empty input, got %+v ok=%v", id, ok)
	}
}

func TestParseIdentityMalformedFieldsDegradeGracefully(t *testing.T) {
	// GMW-ID is well-formed (required for "found"); every other line is
	// broken in some way. None of it should panic or block detection.
	nc := []byte(
		"(GMW-ID V1)\n" +
			"(GMW-JOB)\n" + // no job token at all
			"(GMW-TOOLS ABC)\n" + // non-numeric tool count
			"(GMW-SHA )\n" + // empty sha
			"garbage garbage\n" +
			"N10 G20\n",
	)
	id, ok := ParseIdentity(nc)
	if !ok {
		t.Fatalf("expected identity found despite malformed fields")
	}
	if id.Job != "" || id.Operation != "" {
		t.Errorf("expected empty job/op from malformed line, got %q/%q", id.Job, id.Operation)
	}
	if id.ToolCount != 0 {
		t.Errorf("expected zero tool count from malformed line, got %d", id.ToolCount)
	}
	if id.HeaderSHA256 != "" {
		t.Errorf("expected empty header sha from malformed line, got %q", id.HeaderSHA256)
	}
	if id.SHAMatch {
		t.Errorf("expected no sha match when header sha absent")
	}
	// "garbage garbage" isn't a GMW- line, so it ends the header block —
	// the malformed GMW-JOB/TOOLS/SHA lines above it are still consumed
	// as header lines even though their payload didn't parse.
	if id.HeaderLineCount != 4 {
		t.Errorf("header line count = %d, want 4", id.HeaderLineCount)
	}
}

func TestParseIdentityShaMismatch(t *testing.T) {
	nc := []byte("(GMW-ID V1)\n(GMW-SHA DEADBEEF)\nN10 G20\n")
	id, ok := ParseIdentity(nc)
	if !ok {
		t.Fatalf("expected identity found")
	}
	if id.HeaderSHA256 != "DEADBEEF" {
		t.Errorf("header sha = %q", id.HeaderSHA256)
	}
	if id.SHAMatch {
		t.Errorf("expected sha mismatch (header is not a real sha256 of the body)")
	}
}

func TestParseIdentityJobWithoutOperation(t *testing.T) {
	nc := []byte("(GMW-ID V1)\n(GMW-JOB J000099)\nN10 G20\n")
	id, ok := ParseIdentity(nc)
	if !ok {
		t.Fatalf("expected identity found")
	}
	if id.Job != "J000099" {
		t.Errorf("job = %q", id.Job)
	}
	if id.Operation != "" {
		t.Errorf("operation = %q, want empty", id.Operation)
	}
}

func TestParseIdentityHeaderMustStartAtLineOne(t *testing.T) {
	// A GMW-ID line that isn't the very first line doesn't count —
	// the header block owns line 1 unconditionally.
	nc := []byte("(SOME OTHER COMMENT)\n(GMW-ID V1)\nN10 G20\n")
	id, ok := ParseIdentity(nc)
	if ok || id != nil {
		t.Fatalf("expected no identity when GMW-ID isn't line 1, got %+v ok=%v", id, ok)
	}
}

// ── LoadSidecar ──────────────────────────────────────────────────────────

func TestLoadSidecarRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "prog.nc.gmw.json")
	sc := Sidecar{
		SchemaVersion: 1,
		Job:           "J000020",
		Operation:     "OP10",
		SHA256:        "abc123",
		Tools: []SidecarTool{
			{TNumber: 5, Diameter: 0.25, FluteLength: 0.75, StickoutLength: 1.5, MaxDepth: 1.0},
		},
	}
	buf, err := json.Marshal(&sc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := LoadSidecar(path)
	if err != nil {
		t.Fatalf("LoadSidecar: %v", err)
	}
	if got.Job != "J000020" || got.Operation != "OP10" {
		t.Errorf("job/op = %q/%q", got.Job, got.Operation)
	}
	if len(got.Tools) != 1 || got.Tools[0].TNumber != 5 {
		t.Errorf("tools = %+v", got.Tools)
	}
}

func TestLoadSidecarMissing(t *testing.T) {
	_, err := LoadSidecar(filepath.Join(t.TempDir(), "nope.gmw.json"))
	if err == nil {
		t.Fatalf("expected error for missing sidecar")
	}
	if !os.IsNotExist(err) {
		t.Errorf("expected a not-exist error, got %v", err)
	}
}

func TestLoadSidecarCorrupt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.gmw.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := LoadSidecar(path)
	if err == nil {
		t.Fatalf("expected parse error for corrupt sidecar")
	}
}

func TestSidecarPath(t *testing.T) {
	if got := SidecarPath("/foo/bar/prog.nc"); got != "/foo/bar/prog.nc.gmw.json" {
		t.Errorf("SidecarPath = %q", got)
	}
}

// ── ReconcileTools ───────────────────────────────────────────────────────

func mkLoadedSlot(n int, dia, length float64) ToolTableSlot {
	return ToolTableSlot{Slot: n, EffectiveDiameter: ptrF(dia), EffectiveLength: ptrF(length)}
}

func TestReconcilePocketMatch(t *testing.T) {
	sc := &Sidecar{Tools: []SidecarTool{
		{TNumber: 5, Diameter: 0.25, FluteLength: 0.75, StickoutLength: 1.5, MaxDepth: 0.5},
	}}
	table := &ToolTable{Slots: []ToolTableSlot{mkLoadedSlot(5, 0.25, 2.0)}}

	report := ReconcileTools(sc, table, ReconcileConfig{})
	if len(report.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(report.Tools))
	}
	r := report.Tools[0]
	if r.PocketStatus != PocketMatch {
		t.Errorf("status = %q, want match", r.PocketStatus)
	}
	if r.DiameterWarning {
		t.Errorf("unexpected diameter warning: %s", r.DiameterReason)
	}
	if r.LengthInsufficient {
		t.Errorf("unexpected length flag: %s", r.LengthReason)
	}
	if report.Summary.Match != 1 {
		t.Errorf("summary.match = %d, want 1", report.Summary.Match)
	}
}

func TestReconcilePocketMoved(t *testing.T) {
	sc := &Sidecar{Tools: []SidecarTool{
		{TNumber: 5, Diameter: 0.25, FluteLength: 0.75, StickoutLength: 1.5, MaxDepth: 0.5},
	}}
	// T5's pocket is empty; T12 happens to carry a 0.25" tool instead.
	table := &ToolTable{Slots: []ToolTableSlot{
		{Slot: 5, Empty: true},
		mkLoadedSlot(12, 0.25, 2.0),
	}}

	report := ReconcileTools(sc, table, ReconcileConfig{})
	r := report.Tools[0]
	if r.PocketStatus != PocketMoved {
		t.Fatalf("status = %q, want moved", r.PocketStatus)
	}
	if r.ActualPocket == nil || *r.ActualPocket != 12 {
		t.Errorf("actual pocket = %v, want 12", r.ActualPocket)
	}
	if report.Summary.Moved != 1 {
		t.Errorf("summary.moved = %d, want 1", report.Summary.Moved)
	}
}

func TestReconcilePocketMissing(t *testing.T) {
	sc := &Sidecar{Tools: []SidecarTool{
		{TNumber: 5, Diameter: 0.25, FluteLength: 0.75, StickoutLength: 1.5, MaxDepth: 0.5},
	}}
	// Nothing in the table matches 0.25" anywhere.
	table := &ToolTable{Slots: []ToolTableSlot{
		{Slot: 5, Empty: true},
		mkLoadedSlot(12, 0.5, 3.0),
	}}

	report := ReconcileTools(sc, table, ReconcileConfig{})
	r := report.Tools[0]
	if r.PocketStatus != PocketMissing {
		t.Fatalf("status = %q, want missing", r.PocketStatus)
	}
	if r.PocketReason == "" {
		t.Errorf("expected a pocket reason")
	}
	if report.Summary.Missing != 1 {
		t.Errorf("summary.missing = %d, want 1", report.Summary.Missing)
	}
}

func TestReconcileMissingWhenTableNil(t *testing.T) {
	sc := &Sidecar{Tools: []SidecarTool{{TNumber: 5, Diameter: 0.25}}}
	report := ReconcileTools(sc, nil, ReconcileConfig{})
	if report.Tools[0].PocketStatus != PocketMissing {
		t.Fatalf("status = %q, want missing with nil table", report.Tools[0].PocketStatus)
	}
}

func TestReconcileLengthInsufficientExceedsFlute(t *testing.T) {
	sc := &Sidecar{Tools: []SidecarTool{
		// max_depth 0.5 + default clearance 0.100 = 0.6 required reach,
		// which exceeds the 0.5 flute length.
		{TNumber: 5, Diameter: 0.25, FluteLength: 0.5, StickoutLength: 2.0, MaxDepth: 0.5},
	}}
	table := &ToolTable{Slots: []ToolTableSlot{mkLoadedSlot(5, 0.25, 3.0)}}

	report := ReconcileTools(sc, table, ReconcileConfig{})
	r := report.Tools[0]
	if !r.LengthInsufficient {
		t.Fatalf("expected length_insufficient, reason=%q", r.LengthReason)
	}
	if !strings.Contains(r.LengthReason, "flute length") {
		t.Errorf("reason = %q, want mention of flute length", r.LengthReason)
	}
	if report.Summary.LengthInsufficient != 1 {
		t.Errorf("summary.length_insufficient = %d, want 1", report.Summary.LengthInsufficient)
	}
}

func TestReconcileLengthInsufficientExceedsStickout(t *testing.T) {
	sc := &Sidecar{Tools: []SidecarTool{
		// flute is generous, but stickout is short: reach exceeds it.
		{TNumber: 5, Diameter: 0.25, FluteLength: 5.0, StickoutLength: 0.3, MaxDepth: 0.5},
	}}
	table := &ToolTable{Slots: []ToolTableSlot{mkLoadedSlot(5, 0.25, 3.0)}}

	report := ReconcileTools(sc, table, ReconcileConfig{})
	r := report.Tools[0]
	if !r.LengthInsufficient {
		t.Fatalf("expected length_insufficient, reason=%q", r.LengthReason)
	}
	if !strings.Contains(r.LengthReason, "stickout") {
		t.Errorf("reason = %q, want mention of stickout", r.LengthReason)
	}
}

func TestReconcileLengthInsufficientMachineOffsetShort(t *testing.T) {
	sc := &Sidecar{Tools: []SidecarTool{
		{TNumber: 5, Diameter: 0.25, FluteLength: 5.0, StickoutLength: 5.0, MaxDepth: 2.0},
	}}
	// Required reach = 2.0 + 0.100 = 2.1, but the pocket's machine
	// length offset is only 1.0 — physically too short even though the
	// sidecar's own flute/stickout numbers look fine.
	table := &ToolTable{Slots: []ToolTableSlot{mkLoadedSlot(5, 0.25, 1.0)}}

	report := ReconcileTools(sc, table, ReconcileConfig{})
	r := report.Tools[0]
	if !r.LengthInsufficient {
		t.Fatalf("expected length_insufficient, reason=%q", r.LengthReason)
	}
	if !strings.Contains(r.LengthReason, "geometry length offset") {
		t.Errorf("reason = %q, want mention of geometry length offset", r.LengthReason)
	}
	if r.MachineLengthOffset == nil || *r.MachineLengthOffset != 1.0 {
		t.Errorf("machine length offset = %v, want 1.0", r.MachineLengthOffset)
	}
}

func TestReconcileDiameterWarning(t *testing.T) {
	sc := &Sidecar{Tools: []SidecarTool{
		{TNumber: 5, Diameter: 0.25, FluteLength: 2.0, StickoutLength: 2.0, MaxDepth: 0.1},
	}}
	// Pocket 5 is loaded (so it's a "match"), but the diameter is off
	// by more than tolerance -- a different tool physically sitting
	// in the right slot.
	table := &ToolTable{Slots: []ToolTableSlot{mkLoadedSlot(5, 0.3125, 2.0)}}

	report := ReconcileTools(sc, table, ReconcileConfig{})
	r := report.Tools[0]
	if r.PocketStatus != PocketMatch {
		t.Fatalf("status = %q, want match (tool IS loaded, just wrong)", r.PocketStatus)
	}
	if !r.DiameterWarning {
		t.Fatalf("expected diameter warning, got none")
	}
	if report.Summary.DiameterWarning != 1 {
		t.Errorf("summary.diameter_warning = %d, want 1", report.Summary.DiameterWarning)
	}
}

func TestReconcileDiameterWithinToleranceNoWarning(t *testing.T) {
	sc := &Sidecar{Tools: []SidecarTool{
		{TNumber: 5, Diameter: 0.2500, FluteLength: 2.0, StickoutLength: 2.0, MaxDepth: 0.1},
	}}
	// 0.0005" off -- inside the default 0.005" tolerance shared with
	// preflight.
	table := &ToolTable{Slots: []ToolTableSlot{mkLoadedSlot(5, 0.2505, 2.0)}}

	report := ReconcileTools(sc, table, ReconcileConfig{})
	if report.Tools[0].DiameterWarning {
		t.Errorf("unexpected diameter warning within tolerance: %s", report.Tools[0].DiameterReason)
	}
}

func TestReconcileCustomClearanceMargin(t *testing.T) {
	sc := &Sidecar{Tools: []SidecarTool{
		{TNumber: 5, Diameter: 0.25, FluteLength: 1.0, StickoutLength: 1.0, MaxDepth: 0.5},
	}}
	table := &ToolTable{Slots: []ToolTableSlot{mkLoadedSlot(5, 0.25, 3.0)}}

	// Default clearance (0.1) keeps required reach at 0.6, under the
	// 1.0 flute/stickout -- no flag.
	def := ReconcileTools(sc, table, ReconcileConfig{})
	if def.Tools[0].LengthInsufficient {
		t.Fatalf("expected no length flag at default clearance, got %s", def.Tools[0].LengthReason)
	}

	// A large explicit clearance margin pushes required reach past
	// flute/stickout.
	wide := ReconcileTools(sc, table, ReconcileConfig{ClearanceMargin: 0.75})
	if !wide.Tools[0].LengthInsufficient {
		t.Fatalf("expected length flag with a wide clearance margin")
	}
}

func TestReconcileEmptySidecar(t *testing.T) {
	report := ReconcileTools(&Sidecar{}, &ToolTable{}, ReconcileConfig{})
	if len(report.Tools) != 0 {
		t.Fatalf("expected no tools, got %d", len(report.Tools))
	}
}

func TestReconcileNilSidecar(t *testing.T) {
	report := ReconcileTools(nil, &ToolTable{}, ReconcileConfig{})
	if report == nil || len(report.Tools) != 0 {
		t.Fatalf("expected empty report for nil sidecar, got %+v", report)
	}
}

// ── BuildIdentityReport ──────────────────────────────────────────────────

func TestBuildIdentityReportNoHeader(t *testing.T) {
	nc := []byte("N10 G20\nN20 T5 M06\n")
	if rep := BuildIdentityReport("/tmp/whatever.nc", nc, nil); rep != nil {
		t.Fatalf("expected nil report for un-annotated program, got %+v", rep)
	}
}

func TestBuildIdentityReportSidecarMissing(t *testing.T) {
	dir := t.TempDir()
	ncPath := filepath.Join(dir, "prog.nc")
	nc := []byte("(GMW-ID V1)\n(GMW-JOB J1 OP10)\nN10 G20\n")

	rep := BuildIdentityReport(ncPath, nc, nil)
	if rep == nil {
		t.Fatalf("expected a report when header is present")
	}
	if rep.SidecarFound {
		t.Errorf("expected sidecar not found")
	}
	if rep.SidecarError == "" {
		t.Errorf("expected a sidecar error message")
	}
	if len(rep.Tools) != 0 {
		t.Errorf("expected no tools when sidecar missing")
	}
	if rep.Job != "J1" {
		t.Errorf("job = %q", rep.Job)
	}
}

func TestBuildIdentityReportWithSidecar(t *testing.T) {
	dir := t.TempDir()
	ncPath := filepath.Join(dir, "prog.nc")
	body := "N10 G20\nN20 T5 M06\n"
	sum := sha256.Sum256([]byte(body))
	sha := strings.ToUpper(hex.EncodeToString(sum[:]))
	nc := []byte("(GMW-ID V1)\n(GMW-SHA " + sha + ")\nN10 G20\nN20 T5 M06\n")
	// Note: the sha above is computed over "N10 G20\nN20 T5 M06\n", the
	// full body once the two header lines are stripped.

	sc := Sidecar{
		SchemaVersion: 1,
		Job:           "J000020",
		Tools: []SidecarTool{
			{TNumber: 5, Diameter: 0.25, FluteLength: 1.0, StickoutLength: 1.0, MaxDepth: 0.1},
		},
	}
	buf, err := json.Marshal(&sc)
	if err != nil {
		t.Fatalf("marshal sidecar: %v", err)
	}
	if err := os.WriteFile(SidecarPath(ncPath), buf, 0o644); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}

	table := &ToolTable{Slots: []ToolTableSlot{mkLoadedSlot(5, 0.25, 2.0)}}
	rep := BuildIdentityReport(ncPath, nc, table)
	if rep == nil {
		t.Fatalf("expected a report")
	}
	if !rep.SidecarFound {
		t.Fatalf("expected sidecar found")
	}
	if !rep.SHAMatch {
		t.Errorf("expected sha match, header=%s computed=%s", rep.HeaderSHA256, rep.ComputedSHA256)
	}
	if len(rep.Tools) != 1 || rep.Tools[0].PocketStatus != PocketMatch {
		t.Fatalf("expected 1 matched tool, got %+v", rep.Tools)
	}
	if rep.Summary.Match != 1 {
		t.Errorf("summary.match = %d, want 1", rep.Summary.Match)
	}
}

// ── BuildPreflight integration ───────────────────────────────────────────

func writeTempNC(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "prog.nc")
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write nc: %v", err)
	}
	return path
}

func TestBuildPreflightWithoutIdentityHeader(t *testing.T) {
	path := writeTempNC(t, "(T5 D=0.25 - flat end mill)\nN10 G20\nN20 T5 M06\n")
	pf, err := BuildPreflight(path, "prog.nc", "mill-1", nil, nil)
	if err != nil {
		t.Fatalf("BuildPreflight: %v", err)
	}
	if pf.Identity != nil {
		t.Fatalf("expected nil Identity for un-annotated program, got %+v", pf.Identity)
	}
	// Existing T-number-only behavior must be completely unaffected.
	if len(pf.Tools) != 1 || pf.Tools[0].Tool != 5 {
		t.Fatalf("expected legacy tool usage parse to still work, got %+v", pf.Tools)
	}
}

func TestBuildPreflightWithIdentityHeaderAndSidecar(t *testing.T) {
	body := "N10 G20\nN20 T5 M06\n"
	sum := sha256.Sum256([]byte(body))
	sha := strings.ToUpper(hex.EncodeToString(sum[:]))
	contents := "(GMW-ID V1)\n(GMW-JOB J000020 OP10)\n(GMW-SHA " + sha + ")\n" + body

	path := writeTempNC(t, contents)
	sc := Sidecar{
		SchemaVersion: 1,
		Job:           "J000020",
		Tools: []SidecarTool{
			{TNumber: 5, Diameter: 0.25, FluteLength: 1.0, StickoutLength: 1.0, MaxDepth: 0.1},
		},
	}
	buf, _ := json.Marshal(&sc)
	if err := os.WriteFile(SidecarPath(path), buf, 0o644); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}

	table := &ToolTable{Slots: []ToolTableSlot{mkLoadedSlot(5, 0.25, 2.0)}}
	pf, err := BuildPreflight(path, "prog.nc", "mill-1", table, nil)
	if err != nil {
		t.Fatalf("BuildPreflight: %v", err)
	}
	if pf.Identity == nil {
		t.Fatalf("expected an Identity section")
	}
	if pf.Identity.Job != "J000020" {
		t.Errorf("identity job = %q", pf.Identity.Job)
	}
	if !pf.Identity.SHAMatch {
		t.Errorf("expected sha match")
	}
	if len(pf.Identity.Tools) != 1 || pf.Identity.Tools[0].PocketStatus != PocketMatch {
		t.Fatalf("expected 1 matched identity tool, got %+v", pf.Identity.Tools)
	}
	// Legacy Tools field is untouched -- T5 parsed from the T-code
	// itself (no comment header this time), status resolved against
	// the same table via the pre-existing path.
	if len(pf.Tools) != 1 || pf.Tools[0].Tool != 5 || pf.Tools[0].Status != "ok" {
		t.Fatalf("expected legacy tool path unaffected, got %+v", pf.Tools)
	}
}

func TestParseIdentityShaPendingIsUnstamped(t *testing.T) {
	nc := []byte("(GMW-ID V1)\n(GMW-JOB J000020 OP10)\n(GMW-SHA PENDING)\nG20 G17\nM30\n")
	id, ok := ParseIdentity(nc)
	if !ok {
		t.Fatal("expected a header")
	}
	if id.SHAStatus != SHAUnstamped || id.SHAMatch || id.HeaderSHA256 != "" {
		t.Fatalf("PENDING must read as unstamped, got status=%q match=%v header=%q", id.SHAStatus, id.SHAMatch, id.HeaderSHA256)
	}
	if id.ComputedSHA256 == "" {
		t.Fatal("computed sha must still be present")
	}
}

func TestParseIdentityShaStatusValues(t *testing.T) {
	body := "G20 G17\nM30\n"
	stamped := []byte("(GMW-ID V1)\n(GMW-SHA 0000000000000000000000000000000000000000000000000000000000000000)\n" + body)
	id, _ := ParseIdentity(stamped)
	if id.SHAStatus != SHAMismatch {
		t.Fatalf("wrong hash must be mismatch, got %q", id.SHAStatus)
	}
	good := []byte("(GMW-ID V1)\n(GMW-SHA " + id.ComputedSHA256 + ")\n" + body)
	id2, _ := ParseIdentity(good)
	if id2.SHAStatus != SHAMatchStatus || !id2.SHAMatch {
		t.Fatalf("matching hash must be match, got %q", id2.SHAStatus)
	}
	none := []byte("(GMW-ID V1)\n" + body)
	id3, _ := ParseIdentity(none)
	if id3.SHAStatus != SHAUnstamped {
		t.Fatalf("absent GMW-SHA must be unstamped, got %q", id3.SHAStatus)
	}
}

func TestParseIdentityHeaderAfterTapeMarkerAndONumber(t *testing.T) {
	// What a Haas post actually writes: %, the O-number line, then the
	// header block. The body hash covers everything after the block.
	nc := []byte("%\nO01020 (F-BRACKET)\n(GMW-ID V1)\n(GMW-JOB J000020 OP10)\n(GMW-SHA PENDING)\nG20 G17\nM30\n%\n")
	id, ok := ParseIdentity(nc)
	if !ok || id == nil {
		t.Fatal("header behind % and O-number must be found")
	}
	if id.Job != "J000020" || id.Operation != "OP10" || id.SHAStatus != SHAUnstamped {
		t.Fatalf("unexpected identity: %+v", id)
	}
	if id.HeaderLineCount != 3 {
		t.Fatalf("header line count = %d, want 3", id.HeaderLineCount)
	}
	// A comment before the header is not a preamble.
	if _, ok := ParseIdentity([]byte("%\n(SETUP SHEET)\n(GMW-ID V1)\nM30\n")); ok {
		t.Fatal("a comment before GMW-ID must not count as preamble")
	}
	// Too long a preamble is not scanned.
	if _, ok := ParseIdentity([]byte("%\n\n\n\n(GMW-ID V1)\nM30\n")); ok {
		t.Fatal("preamble longer than maxPreambleLines must not be scanned")
	}
}
