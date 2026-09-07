package cncd

// Tests for registerJobs's routes (router_jobs.go). GET is open (same
// posture as GET /api/files); the two writes require the machine-token
// bearer.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/filebrowser/filebrowser/v2/settings"
)

func TestJobsList_OpenNoAuth(t *testing.T) {
	deps, _ := newTestDeps(t, func(c *settings.Cnc) {
		c.MachineToken = "s3cret"
	})
	writeFile(t, deps.Root, "loose.nc", plainNC())
	jobDir := filepath.Join(deps.Root, "J000020 - F-BRACKET-REV-C")
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, jobDir, "op10.nc", identityNC)

	r := NewRouter(deps)
	req := httptest.NewRequest(http.MethodGet, "/api/jobs", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/jobs (no auth): got %d, want 200 (%s)", rec.Code, rec.Body.String())
	}

	var body JobsListResult
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response not valid JSON: %v (%s)", err, rec.Body.String())
	}
	if len(body.Jobs) != 1 || body.Jobs[0].Name != "J000020 - F-BRACKET-REV-C" {
		t.Fatalf("Jobs = %+v, want one folder", body.Jobs)
	}
	if len(body.Unfiled) != 1 || body.Unfiled[0].Name != "loose.nc" {
		t.Fatalf("Unfiled = %+v, want [loose.nc]", body.Unfiled)
	}
}

func TestJobsCreate_IdempotentAndAuthzGated(t *testing.T) {
	deps, _ := newTestDeps(t, func(c *settings.Cnc) {
		c.MachineToken = "s3cret"
	})
	r := NewRouter(deps)

	post := func(bearer string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/jobs",
			bytes.NewBufferString(`{"job":"J000020","part":"F-BRACKET-REV-C"}`))
		if bearer != "" {
			req.Header.Set("Authorization", bearer)
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}

	// No bearer: forbidden, nothing created.
	rec := post("")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("no bearer: got %d, want 403 (%s)", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(deps.Root, "J000020 - F-BRACKET-REV-C")); !os.IsNotExist(err) {
		t.Fatalf("folder should not exist yet")
	}

	// With bearer: creates.
	rec = post(bearer("s3cret"))
	if rec.Code != http.StatusOK {
		t.Fatalf("create: got %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var first map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &first); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if first["name"] != "J000020 - F-BRACKET-REV-C" || first["created"] != true {
		t.Fatalf("first create body = %+v", first)
	}

	// Calling again is idempotent: same name, created=false.
	rec = post(bearer("s3cret"))
	if rec.Code != http.StatusOK {
		t.Fatalf("second create: got %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var second map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &second); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if second["name"] != "J000020 - F-BRACKET-REV-C" || second["created"] != false {
		t.Fatalf("second create body = %+v, want created=false", second)
	}
}

func TestJobsFile_MovesSidecarAndAuthzGated(t *testing.T) {
	deps, _ := newTestDeps(t, func(c *settings.Cnc) {
		c.MachineToken = "s3cret"
	})
	writeFile(t, deps.Root, "op10.nc", identityNC)
	writeFile(t, deps.Root, "op10.nc.gmw.json", `{"schema_version":1,"job":"J000020","tools":[]}`)
	r := NewRouter(deps)

	fileReq := func(bearer string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/jobs/file",
			bytes.NewBufferString(`{"path":"/op10.nc"}`))
		if bearer != "" {
			req.Header.Set("Authorization", bearer)
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}

	// No bearer: forbidden, file untouched.
	rec := fileReq("")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("no bearer: got %d, want 403 (%s)", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(deps.Root, "op10.nc")); err != nil {
		t.Fatalf("op10.nc should still be at root: %v", err)
	}

	// With bearer: files it, sidecar rides along.
	rec = fileReq(bearer("s3cret"))
	if rec.Code != http.StatusOK {
		t.Fatalf("file: got %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if body["path"] != "J000020 - F-BRACKET-REV-C/op10.nc" {
		t.Fatalf("path = %v", body["path"])
	}
	destDir := filepath.Join(deps.Root, "J000020 - F-BRACKET-REV-C")
	if _, err := os.Stat(filepath.Join(destDir, "op10.nc")); err != nil {
		t.Errorf("expected op10.nc at destination: %v", err)
	}
	if _, err := os.Stat(filepath.Join(destDir, "op10.nc.gmw.json")); err != nil {
		t.Errorf("expected sidecar at destination: %v", err)
	}
}

func TestJobsFile_RejectsFileAlreadyInsideAFolder(t *testing.T) {
	deps, _ := newTestDeps(t, func(c *settings.Cnc) {
		c.MachineToken = "s3cret"
	})
	nested := filepath.Join(deps.Root, "SOME-FOLDER")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, nested, "already-there.nc", identityNC)
	r := NewRouter(deps)

	req := httptest.NewRequest(http.MethodPost, "/api/jobs/file",
		bytes.NewBufferString(`{"path":"/SOME-FOLDER/already-there.nc"}`))
	req.Header.Set("Authorization", bearer("s3cret"))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400 (%s)", rec.Code, rec.Body.String())
	}
}
