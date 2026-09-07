package cncd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/filebrowser/filebrowser/v2/cnc"
	"github.com/filebrowser/filebrowser/v2/settings"
)

// newTestDeps builds a Deps with one configured machine (empty Host,
// so the registry's background Link never dials anywhere real — see
// cnc.Streamer.resolveMachine, which returns ErrConfigMissing for an
// empty Host before any network call happens) and a fresh temp
// directory as the served root.
func newTestDeps(t *testing.T, mutate func(*settings.Cnc)) (Deps, *Store) {
	t.Helper()
	root := t.TempDir()
	cfgPath := filepath.Join(t.TempDir(), "cncd.json")

	store, err := LoadStore(cfgPath)
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	if err := store.Update(func(c *settings.Cnc) error {
		c.Machines = []settings.Machine{{ID: "m1", Name: "Machine 1"}}
		if mutate != nil {
			mutate(c)
		}
		return nil
	}); withErr(t, err) {
		return Deps{}, nil
	}

	registry := cnc.NewRegistry(store)
	t.Cleanup(registry.Stop)

	return Deps{Registry: registry, Config: store, Root: root}, store
}

// withErr is a tiny helper so newTestDeps's Update callback (which
// returns nothing, matching every real caller in this package) can
// still be wrapped in a t.Fatalf without an extra branch at every
// call site. Returns true (caller should return early) on error.
func withErr(t *testing.T, err error) bool {
	t.Helper()
	if err != nil {
		t.Fatalf("store.Update: %v", err)
		return true
	}
	return false
}

func TestState_BearerRequired(t *testing.T) {
	deps, store := newTestDeps(t, func(c *settings.Cnc) {
		c.MachineToken = "s3cret"
	})
	_ = store
	r := NewRouter(deps)

	// No Authorization header at all: 401.
	req := httptest.NewRequest(http.MethodGet, "/api/cnc/state", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no bearer: got %d, want 401", rec.Code)
	}

	// Wrong bearer: still 401.
	req = httptest.NewRequest(http.MethodGet, "/api/cnc/state", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong bearer: got %d, want 401", rec.Code)
	}

	// Correct bearer: 200 with a JSON body.
	req = httptest.NewRequest(http.MethodGet, "/api/cnc/state", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("correct bearer: got %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response not valid JSON: %v (%s)", err, rec.Body.String())
	}
}

func TestState_NoMachineTokenConfigured_AlwaysUnauthorized(t *testing.T) {
	// An install that hasn't set a machine token yet shouldn't have
	// every request to a bearer-only route silently accepted.
	deps, _ := newTestDeps(t, nil)
	r := NewRouter(deps)

	req := httptest.NewRequest(http.MethodGet, "/api/cnc/state", nil)
	req.Header.Set("Authorization", "Bearer anything")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", rec.Code)
	}
}

func TestDisplayFetch_UnauthenticatedWhenTokenEmpty(t *testing.T) {
	deps, _ := newTestDeps(t, func(c *settings.Cnc) {
		c.Displays = []settings.Display{{ID: "d1", MachineID: "m1"}} // Token left empty
	})
	r := NewRouter(deps)

	req := httptest.NewRequest(http.MethodGet, "/api/displays/d1", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var body struct {
		Config settings.Display `json:"config"`
		Data   *cnc.ToolList    `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response not valid JSON: %v (%s)", err, rec.Body.String())
	}
	if body.Data == nil {
		t.Fatalf("expected a tool-list payload, got none")
	}
}

func TestDisplayFetch_TokenGatedWhenSet(t *testing.T) {
	deps, _ := newTestDeps(t, func(c *settings.Cnc) {
		c.Displays = []settings.Display{{ID: "d1", MachineID: "m1", Token: "disp-tok"}}
	})
	r := NewRouter(deps)

	// No token presented: unauthorized.
	req := httptest.NewRequest(http.MethodGet, "/api/displays/d1", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", rec.Code)
	}

	// Correct token via query param: OK.
	req = httptest.NewRequest(http.MethodGet, "/api/displays/d1?token=disp-tok", nil)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rec.Code)
	}
}

func TestDisplayFetch_UnknownID(t *testing.T) {
	deps, _ := newTestDeps(t, nil)
	r := NewRouter(deps)

	req := httptest.NewRequest(http.MethodGet, "/api/displays/nope", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", rec.Code)
	}
}

func TestFiles_ListUploadDelete(t *testing.T) {
	deps, _ := newTestDeps(t, func(c *settings.Cnc) {
		c.MachineToken = "s3cret"
	})
	r := NewRouter(deps)

	// GET on empty root: no auth required, empty entries.
	req := httptest.NewRequest(http.MethodGet, "/api/files?path=/", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list root: got %d (%s)", rec.Code, rec.Body.String())
	}

	// PUT without a bearer: forbidden.
	req = httptest.NewRequest(http.MethodPut, "/api/files?path=/part.nc", bytes.NewBufferString("G0 X0\n"))
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("upload without bearer: got %d, want 403", rec.Code)
	}

	// PUT with the correct bearer: succeeds.
	req = httptest.NewRequest(http.MethodPut, "/api/files?path=/part.nc", bytes.NewBufferString("G0 X0\n"))
	req.Header.Set("Authorization", "Bearer s3cret")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("upload with bearer: got %d (%s)", rec.Code, rec.Body.String())
	}

	// GET now lists the uploaded file.
	req = httptest.NewRequest(http.MethodGet, "/api/files?path=/", nil)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	var listing struct {
		Entries []fileEntry `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listing); err != nil {
		t.Fatalf("listing not valid JSON: %v", err)
	}
	if len(listing.Entries) != 1 || listing.Entries[0].Name != "part.nc" {
		t.Fatalf("expected [part.nc], got %+v", listing.Entries)
	}

	// DELETE without a bearer: forbidden.
	req = httptest.NewRequest(http.MethodDelete, "/api/files?path=/part.nc", nil)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("delete without bearer: got %d, want 403", rec.Code)
	}

	// DELETE with the bearer: succeeds.
	req = httptest.NewRequest(http.MethodDelete, "/api/files?path=/part.nc", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete with bearer: got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestFiles_EscapeRejected(t *testing.T) {
	deps, _ := newTestDeps(t, func(c *settings.Cnc) {
		c.MachineToken = "s3cret"
	})
	r := NewRouter(deps)

	req := httptest.NewRequest(http.MethodPut, "/api/files?path=../../etc/passwd", bytes.NewBufferString("x"))
	req.Header.Set("Authorization", "Bearer s3cret")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("escape attempt: got %d, want 400 (%s)", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/files?path=../../etc/passwd", nil)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("escape attempt (GET): got %d, want 400 (%s)", rec.Code, rec.Body.String())
	}
}
