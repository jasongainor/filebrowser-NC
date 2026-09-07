package cncd

// Tests for registerCNC's routes (router_cnc.go): the full
// /api/cnc/* surface beyond the state/qcode/stream/files/displays
// trio already covered by router_test.go. Each family gets at least
// one happy-path case and one negative case — "authz-denied" for
// mutating/admin routes gated on cncapi.Authz, or an equivalent
// edge case (unknown machine, empty result) for the routes that are
// intentionally open (mirrors GET /api/files' own posture: read
// access to a LAN-only daemon isn't gated the way mutation is).
//
// newTestDeps (router_test.go) configures one machine with an empty
// Host, so every test here runs against an "offline stub": the
// registry is real, but cnc.Streamer.resolveMachine returns
// ErrConfigMissing before any network dial, exactly like
// TestState_BearerRequired already relies on. That means most
// mutating routes' "happy path" case observes the request clearing
// the auth gate and reaching real business logic (typically a 400/409
// business error, never a 403), which is the meaningful thing to
// assert without a real controller on the other end.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/filebrowser/filebrowser/v2/settings"
)

const bearerHeader = "Authorization"

func bearer(tok string) string { return "Bearer " + tok }

// resetQueue clears any persisted queue state for the "m1" test
// machine. cnc.NewRegistry always backs its QueueStore with the
// process-wide default directory (resolveQueueDir(), cnc/queue.go) —
// there's no way to inject a temp dir per test — so every test in
// this file that touches the queue shares one on-disk "m1.json"
// across runs. Without this reset, entries persisted by an earlier
// test (or an earlier run of the same test) leak into the next one.
func resetQueue(t *testing.T, deps Deps) {
	t.Helper()
	if qs := deps.Registry.Queues(); qs != nil {
		_ = qs.Reorder("m1", nil)
	}
}

// ---------------------------------------------------------------
// queue
// ---------------------------------------------------------------

func TestQueue_List_Open(t *testing.T) {
	deps, _ := newTestDeps(t, nil)
	resetQueue(t, deps)
	r := NewRouter(deps)

	req := httptest.NewRequest(http.MethodGet, "/api/cnc/queue", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var items []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &items); err != nil {
		t.Fatalf("not valid JSON array: %v (%s)", err, rec.Body.String())
	}
	if len(items) != 0 {
		t.Fatalf("expected empty queue, got %+v", items)
	}
}

func TestQueue_Add_ForbiddenWithoutBearer(t *testing.T) {
	deps, _ := newTestDeps(t, func(c *settings.Cnc) { c.MachineToken = "s3cret" })
	r := NewRouter(deps)

	body, _ := json.Marshal(map[string]string{"file_path": "/part.nc"})
	req := httptest.NewRequest(http.MethodPost, "/api/cnc/queue", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403 (%s)", rec.Code, rec.Body.String())
	}
}

func TestQueue_AddListRemoveReorderPromote_HappyPath(t *testing.T) {
	root := t.TempDir()
	deps, _ := newTestDepsWithRoot(t, root, func(c *settings.Cnc) { c.MachineToken = "s3cret" })
	resetQueue(t, deps)
	t.Cleanup(func() { resetQueue(t, deps) })
	r := NewRouter(deps)
	writeFile(t, root, "part.nc", "O00057\nG0 X0\n")

	// Add.
	body, _ := json.Marshal(map[string]string{"file_path": "/part.nc"})
	req := httptest.NewRequest(http.MethodPost, "/api/cnc/queue", bytes.NewReader(body))
	req.Header.Set(bearerHeader, bearer("s3cret"))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("add: got %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var item struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &item); err != nil || item.ID == "" {
		t.Fatalf("add: bad response %s (err %v)", rec.Body.String(), err)
	}

	// List reflects it.
	req = httptest.NewRequest(http.MethodGet, "/api/cnc/queue", nil)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	var items []map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &items)
	if len(items) != 1 {
		t.Fatalf("expected 1 queued item, got %+v", items)
	}

	// Promote.
	req = httptest.NewRequest(http.MethodPost, "/api/cnc/queue/"+item.ID+"/promote", nil)
	req.Header.Set(bearerHeader, bearer("s3cret"))
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("promote: got %d, want 200 (%s)", rec.Code, rec.Body.String())
	}

	// Reorder (single-item no-op, but exercises the path).
	body, _ = json.Marshal(map[string][]string{"ids": {item.ID}})
	req = httptest.NewRequest(http.MethodPatch, "/api/cnc/queue", bytes.NewReader(body))
	req.Header.Set(bearerHeader, bearer("s3cret"))
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("reorder: got %d, want 200 (%s)", rec.Code, rec.Body.String())
	}

	// Remove.
	req = httptest.NewRequest(http.MethodDelete, "/api/cnc/queue/"+item.ID, nil)
	req.Header.Set(bearerHeader, bearer("s3cret"))
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("remove: got %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------
// status / start / stop / attach / detach
// ---------------------------------------------------------------

func TestStatus_Open(t *testing.T) {
	deps, _ := newTestDeps(t, nil)
	r := NewRouter(deps)

	req := httptest.NewRequest(http.MethodGet, "/api/cnc/status", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
}

func TestStatus_UnknownMachine(t *testing.T) {
	deps, _ := newTestDeps(t, nil)
	r := NewRouter(deps)

	req := httptest.NewRequest(http.MethodGet, "/api/cnc/status?machine_id=nope", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404 (%s)", rec.Code, rec.Body.String())
	}
}

func TestStart_ForbiddenWithoutBearer(t *testing.T) {
	deps, _ := newTestDeps(t, func(c *settings.Cnc) { c.MachineToken = "s3cret" })
	r := NewRouter(deps)

	body, _ := json.Marshal(map[string]string{"file_path": "/part.nc"})
	req := httptest.NewRequest(http.MethodPost, "/api/cnc/start", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403 (%s)", rec.Code, rec.Body.String())
	}
}

func TestStart_PastAuthz_HitsConfigMissing(t *testing.T) {
	root := t.TempDir()
	deps, _ := newTestDepsWithRoot(t, root, func(c *settings.Cnc) { c.MachineToken = "s3cret" })
	r := NewRouter(deps)
	writeFile(t, root, "part.nc", "G0 X0\n")

	body, _ := json.Marshal(map[string]string{"file_path": "/part.nc"})
	req := httptest.NewRequest(http.MethodPost, "/api/cnc/start", bytes.NewReader(body))
	req.Header.Set(bearerHeader, bearer("s3cret"))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	// The stub machine has no Host/Serial configured, so Start fails
	// with ErrConfigMissing (400) — never 403. That's the meaningful
	// assertion here: the bearer cleared the authz gate.
	if rec.Code == http.StatusForbidden {
		t.Fatalf("bearer should have cleared authz, got 403 (%s)", rec.Body.String())
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400 (ErrConfigMissing) (%s)", rec.Code, rec.Body.String())
	}
}

func TestStop_ForbiddenWithoutBearer_ThenOK(t *testing.T) {
	deps, _ := newTestDeps(t, func(c *settings.Cnc) { c.MachineToken = "s3cret" })
	r := NewRouter(deps)

	req := httptest.NewRequest(http.MethodPost, "/api/cnc/stop", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("without bearer: got %d, want 403 (%s)", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/api/cnc/stop", nil)
	req.Header.Set(bearerHeader, bearer("s3cret"))
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("with bearer: got %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
}

func TestAttachDetach_ForbiddenWithoutBearer_ThenOK(t *testing.T) {
	root := t.TempDir()
	deps, _ := newTestDepsWithRoot(t, root, func(c *settings.Cnc) { c.MachineToken = "s3cret" })
	r := NewRouter(deps)
	writeFile(t, root, "part.nc", "G0 X0\n")

	body, _ := json.Marshal(map[string]string{"file_path": "/part.nc"})
	req := httptest.NewRequest(http.MethodPost, "/api/cnc/attach", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("attach without bearer: got %d, want 403 (%s)", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/api/cnc/attach", bytes.NewReader(body))
	req.Header.Set(bearerHeader, bearer("s3cret"))
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("attach with bearer: got %d, want 200 (%s)", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodDelete, "/api/cnc/attach", nil)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("detach without bearer: got %d, want 403 (%s)", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodDelete, "/api/cnc/attach", nil)
	req.Header.Set(bearerHeader, bearer("s3cret"))
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("detach with bearer: got %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------
// preflight / auto-send
// ---------------------------------------------------------------

func TestPreflight_ForbiddenWithoutBearer_ThenOK(t *testing.T) {
	root := t.TempDir()
	deps, _ := newTestDepsWithRoot(t, root, func(c *settings.Cnc) { c.MachineToken = "s3cret" })
	r := NewRouter(deps)
	writeFile(t, root, "part.nc", "G0 X0 T1 M6\n")

	body, _ := json.Marshal(map[string]string{"file_path": "/part.nc"})
	req := httptest.NewRequest(http.MethodPost, "/api/cnc/preflight", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("without bearer: got %d, want 403 (%s)", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/api/cnc/preflight", bytes.NewReader(body))
	req.Header.Set(bearerHeader, bearer("s3cret"))
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("with bearer: got %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
}

func TestAutoSend_ForbiddenWithoutBearer_ThenBlocked(t *testing.T) {
	root := t.TempDir()
	deps, _ := newTestDepsWithRoot(t, root, func(c *settings.Cnc) { c.MachineToken = "s3cret" })
	r := NewRouter(deps)
	writeFile(t, root, "part.nc", "G0 X0\n")

	body, _ := json.Marshal(map[string]string{"file_path": "/part.nc"})
	req := httptest.NewRequest(http.MethodPost, "/api/cnc/auto-send", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("without bearer: got %d, want 403 (%s)", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/api/cnc/auto-send", bytes.NewReader(body))
	req.Header.Set(bearerHeader, bearer("s3cret"))
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("with bearer: got %d, want 200 (blocked — AutoSendEnabled is false) (%s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Started       bool   `json:"started"`
		BlockedReason string `json:"blocked_reason"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad JSON: %v (%s)", err, rec.Body.String())
	}
	if resp.Started || resp.BlockedReason == "" {
		t.Fatalf("expected a blocked (not started) response, got %+v", resp)
	}
}

// ---------------------------------------------------------------
// tool table
// ---------------------------------------------------------------

func TestToolTableReadLive_AdminRequired(t *testing.T) {
	deps, _ := newTestDeps(t, func(c *settings.Cnc) { c.MachineToken = "s3cret" })
	r := NewRouter(deps)

	req := httptest.NewRequest(http.MethodPost, "/api/cnc/tool-table", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("without bearer: got %d, want 403 (%s)", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/api/cnc/tool-table", nil)
	req.Header.Set(bearerHeader, bearer("s3cret"))
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	// Bearer clears authz; the stub machine has no Host/Serial so the
	// live read itself fails (never 403).
	if rec.Code == http.StatusForbidden {
		t.Fatalf("bearer should have cleared authz, got 403 (%s)", rec.Body.String())
	}
}

func TestToolTableLatest_NoContentThenHistoryEmpty(t *testing.T) {
	deps, _ := newTestDeps(t, nil)
	r := NewRouter(deps)

	req := httptest.NewRequest(http.MethodGet, "/api/cnc/tool-table", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("latest: got %d, want 204 (%s)", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/cnc/tool-table/history", nil)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("history: got %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var body struct {
		Entries []any `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("bad JSON: %v (%s)", err, rec.Body.String())
	}
	if len(body.Entries) != 0 {
		t.Fatalf("expected empty history, got %+v", body.Entries)
	}
}

func TestToolTableEdit_ForbiddenWithoutBearer_ThenNoHistoryYet(t *testing.T) {
	deps, _ := newTestDeps(t, func(c *settings.Cnc) { c.MachineToken = "s3cret" })
	r := NewRouter(deps)

	body, _ := json.Marshal(map[string]any{"slot": 1, "length_geom": 1.23})
	req := httptest.NewRequest(http.MethodPost, "/api/cnc/tool-table/edit", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("without bearer: got %d, want 403 (%s)", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/api/cnc/tool-table/edit", bytes.NewReader(body))
	req.Header.Set(bearerHeader, bearer("s3cret"))
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	// No tool-table has ever been read for this machine, so the edit
	// is refused with 400 — again, never 403.
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("with bearer: got %d, want 400 (no history yet) (%s)", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------
// tool list / tool library
// ---------------------------------------------------------------

func TestMachineToolList_OpenAndUnknown(t *testing.T) {
	deps, _ := newTestDeps(t, nil)
	r := NewRouter(deps)

	req := httptest.NewRequest(http.MethodGet, "/api/machines/m1/toollist", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("known machine: got %d, want 200 (%s)", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/machines/nope/toollist", nil)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown machine: got %d, want 404 (%s)", rec.Code, rec.Body.String())
	}
}

func TestToolLibrary_GetOpen_PutAdminRequired(t *testing.T) {
	deps, _ := newTestDeps(t, func(c *settings.Cnc) { c.MachineToken = "s3cret" })
	r := NewRouter(deps)

	req := httptest.NewRequest(http.MethodGet, "/api/cnc/tool-library", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("get: got %d, want 200 (%s)", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPut, "/api/cnc/tool-library", bytes.NewReader([]byte(`{}`)))
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("put without bearer: got %d, want 403 (%s)", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------
// machines / settings
// ---------------------------------------------------------------

func TestMachinesList_Open(t *testing.T) {
	deps, _ := newTestDeps(t, nil)
	r := NewRouter(deps)

	req := httptest.NewRequest(http.MethodGet, "/api/cnc/machines", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var body struct {
		Machines  []map[string]any `json:"machines"`
		DefaultID string           `json:"default_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("bad JSON: %v (%s)", err, rec.Body.String())
	}
	if len(body.Machines) != 1 || body.DefaultID == "" {
		t.Fatalf("unexpected body: %+v", body)
	}
}

func TestSettings_AdminRequired_ThenRoundTrips(t *testing.T) {
	deps, _ := newTestDeps(t, func(c *settings.Cnc) { c.MachineToken = "s3cret" })
	r := NewRouter(deps)

	req := httptest.NewRequest(http.MethodGet, "/api/cnc/settings", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("get without bearer: got %d, want 403 (%s)", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/cnc/settings", nil)
	req.Header.Set(bearerHeader, bearer("s3cret"))
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("get with bearer: got %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var cfg settings.Cnc
	if err := json.Unmarshal(rec.Body.Bytes(), &cfg); err != nil {
		t.Fatalf("bad JSON: %v (%s)", err, rec.Body.String())
	}
	if len(cfg.Machines) != 1 {
		t.Fatalf("expected the one configured machine to round-trip, got %+v", cfg.Machines)
	}

	// PUT: round-trip a discord config, verifying settings.Cnc is
	// treated opaquely (no per-field allowlist keeps discord from
	// surviving the PUT). The stub machine needs a transport
	// (host or serial.device) to pass NormalizeMachines' validation
	// on the way back in — it never actually dials anywhere in this
	// test, nothing here calls a streamer method.
	cfg.Machines[0].Host = "10.0.0.5"
	cfg.Discord = settings.DiscordConfig{ChannelID: "chan-1"}
	buf, _ := json.Marshal(cfg)
	req = httptest.NewRequest(http.MethodPut, "/api/cnc/settings", bytes.NewReader(buf))
	req.Header.Set(bearerHeader, bearer("s3cret"))
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("put with bearer: got %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var after settings.Cnc
	if err := json.Unmarshal(rec.Body.Bytes(), &after); err != nil {
		t.Fatalf("bad JSON: %v (%s)", err, rec.Body.String())
	}
	if after.Discord.ChannelID != "chan-1" {
		t.Fatalf("expected discord config to round-trip, got %+v", after.Discord)
	}
}

func TestSettingsToken_AdminRequired(t *testing.T) {
	deps, _ := newTestDeps(t, func(c *settings.Cnc) { c.MachineToken = "s3cret" })
	r := NewRouter(deps)

	req := httptest.NewRequest(http.MethodPost, "/api/cnc/settings/token", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("without bearer: got %d, want 403 (%s)", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/api/cnc/settings/token", nil)
	req.Header.Set(bearerHeader, bearer("s3cret"))
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("with bearer: got %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var body struct {
		MachineToken string `json:"machineToken"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.MachineToken == "" || body.MachineToken == "s3cret" {
		t.Fatalf("expected a freshly minted token, got %+v (err %v)", body, err)
	}
}

// ---------------------------------------------------------------
// jobs / codes / host-stats / recovery / displays
// ---------------------------------------------------------------

func TestJobs_ListAndStats_Open(t *testing.T) {
	deps, _ := newTestDeps(t, nil)
	r := NewRouter(deps)

	req := httptest.NewRequest(http.MethodGet, "/api/cnc/jobs", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("jobs: got %d, want 200 (%s)", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/cnc/jobs/stats", nil)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("jobs/stats: got %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
}

func TestJobs_UnknownMachine(t *testing.T) {
	deps, _ := newTestDeps(t, nil)
	r := NewRouter(deps)

	req := httptest.NewRequest(http.MethodGet, "/api/cnc/jobs?machine_id=nope", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404 (%s)", rec.Code, rec.Body.String())
	}
}

func TestCodes_LookupAndSearch_Open(t *testing.T) {
	deps, _ := newTestDeps(t, nil)
	r := NewRouter(deps)

	req := httptest.NewRequest(http.MethodGet, "/api/cnc/codes/lookup?kind=setting&number=414", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("lookup: got %d, want 200 (%s)", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/cnc/codes/lookup?number=notanumber", nil)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("lookup bad number: got %d, want 400 (%s)", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/cnc/codes/search?q=probe", nil)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("search: got %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
}

func TestHostStats_Open(t *testing.T) {
	deps, _ := newTestDeps(t, nil)
	r := NewRouter(deps)

	req := httptest.NewRequest(http.MethodGet, "/api/cnc/host-stats", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
}

func TestRecoveryAck_ForbiddenWithoutBearer_ThenOK(t *testing.T) {
	deps, _ := newTestDeps(t, func(c *settings.Cnc) { c.MachineToken = "s3cret" })
	r := NewRouter(deps)

	req := httptest.NewRequest(http.MethodPost, "/api/cnc/recovery/ack", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("without bearer: got %d, want 403 (%s)", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/api/cnc/recovery/ack", nil)
	req.Header.Set(bearerHeader, bearer("s3cret"))
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("with bearer: got %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
}

func TestDisplaysCRUD_AdminRequired_ThenHappyPath(t *testing.T) {
	deps, _ := newTestDeps(t, func(c *settings.Cnc) { c.MachineToken = "s3cret" })
	r := NewRouter(deps)

	// List without bearer: forbidden.
	req := httptest.NewRequest(http.MethodGet, "/api/cnc/displays", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("list without bearer: got %d, want 403 (%s)", rec.Code, rec.Body.String())
	}

	// Create without bearer: forbidden.
	body, _ := json.Marshal(map[string]string{"machineId": "m1", "name": "Shop floor"})
	req = httptest.NewRequest(http.MethodPost, "/api/cnc/displays", bytes.NewReader(body))
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("create without bearer: got %d, want 403 (%s)", rec.Code, rec.Body.String())
	}

	// Create with bearer: OK.
	req = httptest.NewRequest(http.MethodPost, "/api/cnc/displays", bytes.NewReader(body))
	req.Header.Set(bearerHeader, bearer("s3cret"))
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("create with bearer: got %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var disp settings.Display
	if err := json.Unmarshal(rec.Body.Bytes(), &disp); err != nil || disp.ID == "" {
		t.Fatalf("bad create response: %v (%s)", err, rec.Body.String())
	}

	// List with bearer: shows it.
	req = httptest.NewRequest(http.MethodGet, "/api/cnc/displays", nil)
	req.Header.Set(bearerHeader, bearer("s3cret"))
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list with bearer: got %d, want 200 (%s)", rec.Code, rec.Body.String())
	}

	// Update with bearer: OK.
	updateBody, _ := json.Marshal(map[string]string{"machineId": "m1", "name": "Renamed"})
	req = httptest.NewRequest(http.MethodPut, "/api/cnc/displays/"+disp.ID, bytes.NewReader(updateBody))
	req.Header.Set(bearerHeader, bearer("s3cret"))
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("update with bearer: got %d, want 200 (%s)", rec.Code, rec.Body.String())
	}

	// Delete without bearer: forbidden; with bearer: OK.
	req = httptest.NewRequest(http.MethodDelete, "/api/cnc/displays/"+disp.ID, nil)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("delete without bearer: got %d, want 403 (%s)", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodDelete, "/api/cnc/displays/"+disp.ID, nil)
	req.Header.Set(bearerHeader, bearer("s3cret"))
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete with bearer: got %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
}
