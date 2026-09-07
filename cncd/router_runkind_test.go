package cncd

// Tests for registerRunKind's routes (router_runkind.go): POST/GET
// /api/cnc/run-kind. Follows router_cnc_test.go's conventions —
// newTestDeps/newTestDepsWithRoot, bearer() for the machine-token
// path — plus a small local gmw-mes stub (runKindStubServer) so the
// happy-path tests can actually get a run open via POST
// /api/cnc/attach, which is the one existing route that opens a
// cnc.Reporter run without needing a live machine connection (see
// cnc.Streamer.Attach: no network dial, just in-memory state +
// EmitStatus()).

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/filebrowser/filebrowser/v2/cnc"
	"github.com/filebrowser/filebrowser/v2/settings"
)

// runKindStubReq is one request the stub gmw-mes server received.
type runKindStubReq struct {
	method string
	path   string
	body   map[string]any
}

// runKindStubServer builds a fake gmw-mes that accepts any
// POST /api/machine/runs (returning a run id) and any PATCH, pushing
// every request it sees onto the returned channel.
func runKindStubServer(t *testing.T) (*httptest.Server, <-chan runKindStubReq) {
	t.Helper()
	ch := make(chan runKindStubReq, 32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf, _ := io.ReadAll(r.Body)
		var body map[string]any
		if len(buf) > 0 {
			_ = json.Unmarshal(buf, &body)
		}
		if r.Method == http.MethodPost && r.URL.Path == "/api/machine/runs" {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"run":{"id":"run-rk-1"},"linked":false}`))
		} else {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"run":{},"carbon":{"attempted":false}}`))
		}
		ch <- runKindStubReq{method: r.Method, path: r.URL.Path, body: body}
	}))
	t.Cleanup(srv.Close)
	return srv, ch
}

func waitRunKindReq(t *testing.T, ch <-chan runKindStubReq) runKindStubReq {
	t.Helper()
	select {
	case req := <-ch:
		return req
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a request to the stub gmw-mes server")
		return runKindStubReq{}
	}
}

// openRunViaAttach opens a cnc.Reporter run for "m1" by attaching a
// staged file — the offline-stub-friendly path described above — and
// waits for the resulting POST /api/machine/runs to land AND for the
// reporter to have actually recorded the run_id from that response
// (Reporter.OpenRunInfo polled with a short timeout) before returning
// — the HTTP round trip to the stub server and the reporter's own
// json.Unmarshal/setRunID bookkeeping both happen on the worker
// goroutine, strictly after the request is visible on reqs.
func openRunViaAttach(t *testing.T, deps Deps, r http.Handler, reqs <-chan runKindStubReq) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"file_path": "/part.nc"})
	req := httptest.NewRequest(http.MethodPost, "/api/cnc/attach", bytes.NewReader(body))
	req.Header.Set(bearerHeader, bearer("s3cret"))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("attach: got %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	create := waitRunKindReq(t, reqs)
	if create.method != http.MethodPost || create.path != "/api/machine/runs" {
		t.Fatalf("want the attach to open a run via POST /api/machine/runs, got %s %s", create.method, create.path)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, runID, ok := deps.Registry.Reporter().OpenRunInfo("m1"); ok && runID != "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the reporter to record the new run's run_id")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// newRunKindDeps builds a Deps with machine "m1", a machine token, and
// reporting pointed at a stub gmw-mes server, plus the staged file
// openRunViaAttach needs. root is a fresh temp dir. CNC_STATE_DIR is
// isolated per test (t.Setenv) so one test's run marker can never be
// picked up by another's Reporter.Watch on startup — see
// cnc/reporter_test.go, every test there does the same.
func newRunKindDeps(t *testing.T) (Deps, <-chan runKindStubReq) {
	t.Helper()
	t.Setenv("CNC_STATE_DIR", t.TempDir())
	srv, reqs := runKindStubServer(t)
	root := t.TempDir()
	deps, _ := newTestDepsWithRoot(t, root, func(c *settings.Cnc) {
		c.MachineToken = "s3cret"
		c.Reporting = settings.ReportingConfig{GmwMesURL: srv.URL}
	})
	writeFile(t, root, "part.nc", "O00057\nG0 X0\n")

	// cnc.NewRegistry (inside newTestDepsWithRoot) spins up
	// Reporter.Watch on a background goroutine per machine; it
	// subscribes to the streamer's event feed only after some setup
	// (worker goroutine, restart recovery read). Wait for that
	// subscription before any test emits a status change via
	// /api/cnc/attach, or the emit is simply lost to a subscriber that
	// hasn't registered yet — same race cnc/reporter_test.go's
	// startWatch helper documents and waits out.
	st, _ := deps.Registry.Streamer("m1")
	deadline := time.Now().Add(2 * time.Second)
	for st.SubscriberCount() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("cnc.Reporter did not subscribe to m1's event feed in time")
		}
		time.Sleep(2 * time.Millisecond)
	}
	return deps, reqs
}

// ---------------------------------------------------------------
// POST /api/cnc/run-kind
// ---------------------------------------------------------------

func TestRunKind_Set_ForbiddenWithoutBearer(t *testing.T) {
	t.Setenv("CNC_STATE_DIR", t.TempDir())
	deps, _ := newTestDeps(t, func(c *settings.Cnc) { c.MachineToken = "s3cret" })
	r := NewRouter(deps)

	body, _ := json.Marshal(map[string]string{"kind": cnc.RunKindTrial})
	req := httptest.NewRequest(http.MethodPost, "/api/cnc/run-kind", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403 (%s)", rec.Code, rec.Body.String())
	}
}

func TestRunKind_Set_InvalidKind_BadRequest(t *testing.T) {
	t.Setenv("CNC_STATE_DIR", t.TempDir())
	deps, _ := newTestDeps(t, func(c *settings.Cnc) { c.MachineToken = "s3cret" })
	r := NewRouter(deps)

	for _, bad := range []string{"", "TRIAL", "bogus", "Production"} {
		body, _ := json.Marshal(map[string]string{"kind": bad})
		req := httptest.NewRequest(http.MethodPost, "/api/cnc/run-kind", bytes.NewReader(body))
		req.Header.Set(bearerHeader, bearer("s3cret"))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("kind=%q: got %d, want 400 (%s)", bad, rec.Code, rec.Body.String())
		}
	}
}

func TestRunKind_Set_UnknownMachine_NotFound(t *testing.T) {
	t.Setenv("CNC_STATE_DIR", t.TempDir())
	deps, _ := newTestDeps(t, func(c *settings.Cnc) { c.MachineToken = "s3cret" })
	r := NewRouter(deps)

	body, _ := json.Marshal(map[string]string{"machine_id": "ghost", "kind": cnc.RunKindTrial})
	req := httptest.NewRequest(http.MethodPost, "/api/cnc/run-kind", bytes.NewReader(body))
	req.Header.Set(bearerHeader, bearer("s3cret"))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404 (%s)", rec.Code, rec.Body.String())
	}
}

func TestRunKind_Set_NoRunOpen_Conflict(t *testing.T) {
	t.Setenv("CNC_STATE_DIR", t.TempDir())
	// Reporting isn't even configured here, so cnc.Reporter never
	// tracks an open run for "m1" — same observable outcome (409) as
	// reporting-configured-but-idle, which SetKind can't distinguish
	// and the route doesn't need to.
	deps, _ := newTestDeps(t, func(c *settings.Cnc) { c.MachineToken = "s3cret" })
	r := NewRouter(deps)

	body, _ := json.Marshal(map[string]string{"kind": cnc.RunKindTrial})
	req := httptest.NewRequest(http.MethodPost, "/api/cnc/run-kind", bytes.NewReader(body))
	req.Header.Set(bearerHeader, bearer("s3cret"))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("got %d, want 409 (%s)", rec.Code, rec.Body.String())
	}
}

func TestRunKind_Set_HappyPath(t *testing.T) {
	deps, reqs := newRunKindDeps(t)
	r := NewRouter(deps)
	openRunViaAttach(t, deps, r, reqs)

	body, _ := json.Marshal(map[string]string{"kind": cnc.RunKindProduction})
	req := httptest.NewRequest(http.MethodPost, "/api/cnc/run-kind", bytes.NewReader(body))
	req.Header.Set(bearerHeader, bearer("s3cret"))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var out map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response not valid JSON: %v (%s)", err, rec.Body.String())
	}
	if out["kind"] != cnc.RunKindProduction {
		t.Errorf("kind = %q, want %q", out["kind"], cnc.RunKindProduction)
	}
	if out["machine_id"] != "m1" {
		t.Errorf("machine_id = %q, want m1", out["machine_id"])
	}

	setKind := waitRunKindReq(t, reqs)
	if setKind.method != http.MethodPatch || setKind.path != "/api/machine/runs/run-rk-1" {
		t.Fatalf("want PATCH /api/machine/runs/run-rk-1, got %s %s", setKind.method, setKind.path)
	}
	if setKind.body["kind"] != cnc.RunKindProduction {
		t.Errorf("PATCH body kind = %v, want %q", setKind.body["kind"], cnc.RunKindProduction)
	}
}

// ---------------------------------------------------------------
// GET /api/cnc/run-kind
// ---------------------------------------------------------------

func TestRunKind_Get_UnknownMachine_NotFound(t *testing.T) {
	t.Setenv("CNC_STATE_DIR", t.TempDir())
	deps, _ := newTestDeps(t, nil)
	r := NewRouter(deps)

	req := httptest.NewRequest(http.MethodGet, "/api/cnc/run-kind?machine_id=ghost", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404 (%s)", rec.Code, rec.Body.String())
	}
}

func TestRunKind_Get_NoRunOpen_ReturnsOpenFalse(t *testing.T) {
	t.Setenv("CNC_STATE_DIR", t.TempDir())
	// No bearer at all: GET is an open reader (same posture as GET
	// /api/cnc/status), so this alone must not be a 403/401.
	deps, _ := newTestDeps(t, func(c *settings.Cnc) { c.MachineToken = "s3cret" })
	r := NewRouter(deps)

	req := httptest.NewRequest(http.MethodGet, "/api/cnc/run-kind", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response not valid JSON: %v (%s)", err, rec.Body.String())
	}
	if open, _ := out["open"].(bool); open {
		t.Errorf("open = %v, want false", out["open"])
	}
	if out["kind"] != "" {
		t.Errorf("kind = %v, want empty", out["kind"])
	}
}

func TestRunKind_Get_OpenRun_ReflectsSetKind(t *testing.T) {
	deps, reqs := newRunKindDeps(t)
	r := NewRouter(deps)
	openRunViaAttach(t, deps, r, reqs)

	// Before any override: open, but no kind yet (attach carries no
	// GMW-* identity — see docs/RUN_REPORTING.md).
	req := httptest.NewRequest(http.MethodGet, "/api/cnc/run-kind?machine_id=m1", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if open, _ := out["open"].(bool); !open {
		t.Fatalf("expected open=true, got %+v", out)
	}
	if out["run_id"] != "run-rk-1" {
		t.Errorf("run_id = %v, want run-rk-1", out["run_id"])
	}
	if out["kind"] != "" {
		t.Errorf("kind = %v, want empty before any override", out["kind"])
	}

	// Set it, then re-GET.
	body, _ := json.Marshal(map[string]string{"kind": cnc.RunKindRnd})
	setReq := httptest.NewRequest(http.MethodPost, "/api/cnc/run-kind", bytes.NewReader(body))
	setReq.Header.Set(bearerHeader, bearer("s3cret"))
	setRec := httptest.NewRecorder()
	r.ServeHTTP(setRec, setReq)
	if setRec.Code != http.StatusOK {
		t.Fatalf("set: got %d, want 200 (%s)", setRec.Code, setRec.Body.String())
	}
	waitRunKindReq(t, reqs) // the SetKind PATCH

	req = httptest.NewRequest(http.MethodGet, "/api/cnc/run-kind?machine_id=m1", nil)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	out = nil
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["kind"] != cnc.RunKindRnd {
		t.Errorf("kind = %v, want %q after override", out["kind"], cnc.RunKindRnd)
	}
}

// ---------------------------------------------------------------
// GET /api/cnc/state surfaces run_kind/run_id
// ---------------------------------------------------------------

func TestState_SurfacesRunKindAndRunID_WhenRunOpen(t *testing.T) {
	deps, reqs := newRunKindDeps(t)
	r := NewRouter(deps)
	openRunViaAttach(t, deps, r, reqs)

	body, _ := json.Marshal(map[string]string{"kind": cnc.RunKindTrial})
	setReq := httptest.NewRequest(http.MethodPost, "/api/cnc/run-kind", bytes.NewReader(body))
	setReq.Header.Set(bearerHeader, bearer("s3cret"))
	setRec := httptest.NewRecorder()
	r.ServeHTTP(setRec, setReq)
	if setRec.Code != http.StatusOK {
		t.Fatalf("set: got %d, want 200 (%s)", setRec.Code, setRec.Body.String())
	}
	waitRunKindReq(t, reqs)

	req := httptest.NewRequest(http.MethodGet, "/api/cnc/state", nil)
	req.Header.Set(bearerHeader, bearer("s3cret"))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response not valid JSON: %v (%s)", err, rec.Body.String())
	}
	if out["run_kind"] != cnc.RunKindTrial {
		t.Errorf("run_kind = %v, want %q", out["run_kind"], cnc.RunKindTrial)
	}
	if out["run_id"] != "run-rk-1" {
		t.Errorf("run_id = %v, want run-rk-1", out["run_id"])
	}
	// Existing metric keys must still be present, unchanged in shape.
	if _, ok := out["mode"]; !ok {
		t.Errorf("expected the usual metric keys (e.g. \"mode\") to still be present, got %+v", out)
	}
}

func TestState_OmitsRunKindAndRunID_WhenNoRunOpen(t *testing.T) {
	t.Setenv("CNC_STATE_DIR", t.TempDir())
	deps, _ := newTestDeps(t, func(c *settings.Cnc) { c.MachineToken = "s3cret" })
	r := NewRouter(deps)

	req := httptest.NewRequest(http.MethodGet, "/api/cnc/state", nil)
	req.Header.Set(bearerHeader, bearer("s3cret"))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if _, present := out["run_kind"]; present {
		t.Errorf("expected no run_kind key when nothing is open, got %v", out["run_kind"])
	}
	if _, present := out["run_id"]; present {
		t.Errorf("expected no run_id key when nothing is open, got %v", out["run_id"])
	}
}
