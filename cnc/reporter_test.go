package cnc

// Reporter tests. Every network call goes through an httptest.Server;
// nothing here ever reaches a real gmw-mes. fakeSettings (defined in
// streamer_attach_test.go, same package) supplies the settingsReader
// Reporter and Streamer both take.

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/filebrowser/filebrowser/v2/settings"
)

// capturedReq is one request the fake gmw-mes server received.
type capturedReq struct {
	method string
	path   string
	token  string
	body   map[string]any
}

// captureServer builds an httptest.Server that decodes every JSON body,
// pushes a capturedReq on ch, and lets the test's respond func decide
// the response. respond may be nil for a blanket 200 + empty object.
func captureServer(t *testing.T, respond func(w http.ResponseWriter, r *http.Request, body map[string]any)) (*httptest.Server, <-chan capturedReq) {
	t.Helper()
	ch := make(chan capturedReq, 32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf, _ := io.ReadAll(r.Body)
		var body map[string]any
		if len(buf) > 0 {
			_ = json.Unmarshal(buf, &body)
		}
		cr := capturedReq{method: r.Method, path: r.URL.Path, token: r.Header.Get("X-Bot-Token"), body: body}
		// Respond BEFORE publishing to ch: the test's waitReq() unblocks
		// the instant this is on the channel, and its very next line is
		// often "assert, then stop() which cancels the context feeding
		// this same request." Writing the response first ensures the
		// client's round-trip is complete (from the server's point of
		// view) before that race window opens.
		if respond != nil {
			respond(w, r, body)
		} else {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		}
		ch <- cr
	}))
	t.Cleanup(srv.Close)
	return srv, ch
}

// reportingSettings builds a fakeSettings wired for reporting against
// srv, with an optional machine-id remap.
func reportingSettings(srvURL, tokenEnv string) *fakeSettings {
	return &fakeSettings{s: &settings.Settings{
		Cnc: settings.Cnc{
			Reporting: settings.ReportingConfig{
				GmwMesURL: srvURL,
				TokenEnv:  tokenEnv,
			},
		},
	}}
}

func waitReq(t *testing.T, ch <-chan capturedReq) capturedReq {
	t.Helper()
	select {
	case req := <-ch:
		return req
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a request to the fake gmw-mes server")
		return capturedReq{}
	}
}

func expectNoReq(t *testing.T, ch <-chan capturedReq) {
	t.Helper()
	select {
	case req := <-ch:
		t.Fatalf("unexpected request: %s %s", req.method, req.path)
	case <-time.After(150 * time.Millisecond):
	}
}

var sha256HexRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// gmwStampedProgram writes an NC file carrying a GMW header block to
// dir and returns its absolute path.
func gmwStampedProgram(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "part.nc")
	content := "(GMW-ID V1)\n" +
		"(GMW-JOB J000020 OP10)\n" +
		"(GMW-PART F-BRACKET-REV-C)\n" +
		"(GMW-SHA PENDING)\n" +
		"O0057\n" +
		"G20 G17 G90\n" +
		"M30\n"
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write program: %v", err)
	}
	return p
}

// startWatch runs Reporter.Watch on a background goroutine and returns
// a cancel func that stops it and waits for it to exit.
func startWatch(t *testing.T, r *Reporter, machineID string, st *Streamer) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.Watch(ctx, machineID, st)
	}()
	// Watch's Subscribe() call happens after some setup (spawning the
	// worker, running restart recovery); wait for it so a test's first
	// emit() isn't lost to a subscriber that hasn't registered yet —
	// emit() only fans out to whoever is subscribed at that instant.
	deadline := time.Now().Add(2 * time.Second)
	for st.SubscriberCount() == 0 {
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("Watch did not subscribe to the event feed in time")
		}
		time.Sleep(2 * time.Millisecond)
	}
	return func() {
		// A brief grace period before cancelling: waitReq() unblocks the
		// instant the fake server finishes WRITING a response, which can
		// still be microseconds ahead of the client finishing reading it
		// (io.ReadAll) and the worker doing its post-response bookkeeping
		// (json.Unmarshal / setRunID / persistOpen). Without this, a
		// test that asserts immediately after its last waitReq() and
		// then tears down can genuinely race its own in-flight request,
		// aborting it with "context canceled" — a test-harness artifact,
		// not anything Reporter itself gets wrong.
		time.Sleep(20 * time.Millisecond)
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("Watch did not exit after cancel")
		}
		// Watch exiting only means the event-feed loop stopped; the
		// per-machine worker it spawned may still be mid-flight on a
		// queued HTTP call. Wait for full drain so a test's own
		// t.TempDir()/httptest.Server cleanup can't race it.
		waitDone := make(chan struct{})
		go func() {
			r.Wait()
			close(waitDone)
		}()
		select {
		case <-waitDone:
		case <-time.After(2 * time.Second):
			t.Fatal("Reporter worker did not drain after cancel")
		}
	}
}

// ── open ──────────────────────────────────────────────────────────────

func TestReporter_OpenRun_IdentityFromHeaderStampedFile(t *testing.T) {
	t.Setenv("CNC_STATE_DIR", t.TempDir())
	srv, reqs := captureServer(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"run":{"id":"run-1"},"linked":true}`))
	})

	fs := reportingSettings(srv.URL, "")
	r := NewReporter(fs)
	st := New(fs, "mill-1")
	stop := startWatch(t, r, "mill-1", st)
	defer stop()

	progDir := t.TempDir()
	absPath := gmwStampedProgram(t, progDir)

	startedAt := time.Now().UTC()
	j := &job{id: "job-1", displayPath: "/jobs/part.nc", absPath: absPath, startedAt: startedAt, lineTotal: 3}
	if err := writeMarkerFor("mill-1", j); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	// Seed the O-number cache the way a real status_combined poll would,
	// before the run opens.
	st.emit(Event{Type: "metric", Metric: &Metric{Key: "status_combined", Parsed: map[string]string{
		"program": "O0057", "status": "RUNNING", "parts": "0",
	}}})
	st.emit(Event{Type: "status", Status: &Status{
		Running: true, JobID: "job-1", FilePath: "/jobs/part.nc", StartedAt: startedAt,
	}})

	req := waitReq(t, reqs)
	if req.method != http.MethodPost || req.path != "/api/machine/runs" {
		t.Fatalf("want POST /api/machine/runs, got %s %s", req.method, req.path)
	}
	if req.body["job_readable_id"] != "J000020" {
		t.Errorf("job_readable_id = %v, want J000020", req.body["job_readable_id"])
	}
	if req.body["operation_ref"] != "OP10" {
		t.Errorf("operation_ref = %v, want OP10", req.body["operation_ref"])
	}
	if req.body["part_id"] != "F-BRACKET-REV-C" {
		t.Errorf("part_id = %v, want F-BRACKET-REV-C", req.body["part_id"])
	}
	sha, _ := req.body["program_sha256"].(string)
	if !sha256HexRe.MatchString(sha) {
		t.Errorf("program_sha256 = %q, want 64 lowercase hex chars", sha)
	}
	if req.body["o_number"] != "O0057" {
		t.Errorf("o_number = %v, want O0057", req.body["o_number"])
	}
	if req.body["file_name"] != "part.nc" {
		t.Errorf("file_name = %v, want part.nc", req.body["file_name"])
	}
	if req.body["machine_id"] != "mill-1" {
		t.Errorf("machine_id = %v, want mill-1", req.body["machine_id"])
	}
}

func TestReporter_OpenRun_202UnlinkedStillStoresRunID(t *testing.T) {
	t.Setenv("CNC_STATE_DIR", t.TempDir())
	srv, reqs := captureServer(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/machine/runs":
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"run":{"id":"run-unlinked"},"linked":false}`))
		case r.Method == http.MethodPatch:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"run":{},"carbon":{"attempted":false}}`))
		}
	})

	fs := reportingSettings(srv.URL, "")
	r := NewReporter(fs)
	st := New(fs, "mill-1")
	stop := startWatch(t, r, "mill-1", st)
	defer stop()

	// No marker at all — an attach-only run, unlinked identity.
	st.emit(Event{Type: "status", Status: &Status{
		Running: false, AttachedFile: "/jobs/sd-card.nc", AttachedSource: "auto", AttachedAt: time.Now().UTC(),
	}})

	createReq := waitReq(t, reqs)
	if createReq.path != "/api/machine/runs" {
		t.Fatalf("want create request first, got %s", createReq.path)
	}

	// Close it and confirm the PATCH targets the run_id gmw-mes gave us
	// on the 202 path, proving it was actually stored client-side.
	st.emit(Event{Type: "status", Status: &Status{Running: false, AttachedFile: ""}})
	closeReq := waitReq(t, reqs)
	if closeReq.path != "/api/machine/runs/run-unlinked" || closeReq.method != http.MethodPatch {
		t.Fatalf("want PATCH /api/machine/runs/run-unlinked, got %s %s", closeReq.method, closeReq.path)
	}
}

// ── events ───────────────────────────────────────────────────────────

func TestReporter_PartsEvent(t *testing.T) {
	t.Setenv("CNC_STATE_DIR", t.TempDir())
	srv, reqs := captureServer(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/machine/runs" {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"run":{"id":"run-parts"},"linked":true}`))
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	})

	fs := reportingSettings(srv.URL, "")
	r := NewReporter(fs)
	st := New(fs, "mill-1")
	stop := startWatch(t, r, "mill-1", st)
	defer stop()

	st.emit(Event{Type: "status", Status: &Status{Running: true, JobID: "job-2", FilePath: "/x.nc", StartedAt: time.Now().UTC()}})
	waitReq(t, reqs) // create

	st.emit(Event{Type: "metric", Metric: &Metric{Key: "status_combined", Parsed: map[string]string{
		"program": "O0057", "status": "RUNNING", "parts": "5",
	}}})

	ev := waitReq(t, reqs)
	if ev.path != "/api/machine/runs/run-parts/events" {
		t.Fatalf("want the parts event to post to run-parts, got %s", ev.path)
	}
	if ev.body["type"] != "parts" {
		t.Errorf("type = %v, want parts", ev.body["type"])
	}
	detail, _ := ev.body["detail"].(map[string]any)
	if detail["parts_count"] != float64(5) {
		t.Errorf("parts_count = %v, want 5", detail["parts_count"])
	}
}

func TestReporter_AlarmEvent(t *testing.T) {
	t.Setenv("CNC_STATE_DIR", t.TempDir())
	srv, reqs := captureServer(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/machine/runs" {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"run":{"id":"run-alarm"},"linked":true}`))
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	})

	fs := reportingSettings(srv.URL, "")
	r := NewReporter(fs)
	st := New(fs, "mill-1")
	stop := startWatch(t, r, "mill-1", st)
	defer stop()

	startedAt := time.Now().UTC()
	st.emit(Event{Type: "status", Status: &Status{Running: true, JobID: "job-3", FilePath: "/x.nc", StartedAt: startedAt}})
	waitReq(t, reqs) // create

	st.emit(Event{Type: "status", Status: &Status{
		Running: true, JobID: "job-3", FilePath: "/x.nc", StartedAt: startedAt,
		HaasLastError: "ALARM 401 SERVO ERROR",
	}})

	ev := waitReq(t, reqs)
	if ev.path != "/api/machine/runs/run-alarm/events" {
		t.Fatalf("want the alarm event to post to run-alarm, got %s", ev.path)
	}
	if ev.body["type"] != "alarm" {
		t.Errorf("type = %v, want alarm", ev.body["type"])
	}
	detail, _ := ev.body["detail"].(map[string]any)
	if detail["message"] != "ALARM 401 SERVO ERROR" {
		t.Errorf("message = %v", detail["message"])
	}
}

// ── close ────────────────────────────────────────────────────────────

func TestReporter_CloseWithOutcomeAndParts(t *testing.T) {
	t.Setenv("CNC_STATE_DIR", t.TempDir())
	srv, reqs := captureServer(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/machine/runs" {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"run":{"id":"run-close"},"linked":true}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"run":{},"carbon":{"attempted":false}}`))
	})

	fs := reportingSettings(srv.URL, "")
	r := NewReporter(fs)
	st := New(fs, "mill-1")
	stop := startWatch(t, r, "mill-1", st)
	defer stop()

	startedAt := time.Now().UTC()
	st.emit(Event{Type: "status", Status: &Status{Running: true, JobID: "job-4", FilePath: "/x.nc", StartedAt: startedAt}})
	waitReq(t, reqs) // create

	st.emit(Event{Type: "metric", Metric: &Metric{Key: "status_combined", Parsed: map[string]string{
		"program": "O0057", "status": "RUNNING", "parts": "12",
	}}})
	waitReq(t, reqs) // parts event

	// The streamer's own run() defer appends this before the "running
	// false" status event ever fires — see cnc/job_history.go.
	if err := AppendJobHistory("mill-1", JobHistoryEntry{
		JobID: "job-4", MachineID: "mill-1", StartedAt: startedAt, EndedAt: time.Now().UTC(),
		LineTotal: 10, LineFinal: 10, Status: "completed",
	}); err != nil {
		t.Fatalf("append job history: %v", err)
	}

	st.emit(Event{Type: "status", Status: &Status{Running: false, JobID: "job-4", FilePath: "/x.nc"}})

	closeReq := waitReq(t, reqs)
	if closeReq.path != "/api/machine/runs/run-close" || closeReq.method != http.MethodPatch {
		t.Fatalf("want PATCH /api/machine/runs/run-close, got %s %s", closeReq.method, closeReq.path)
	}
	if closeReq.body["outcome"] != "completed" {
		t.Errorf("outcome = %v, want completed", closeReq.body["outcome"])
	}
	if closeReq.body["parts_count"] != float64(12) {
		t.Errorf("parts_count = %v, want 12", closeReq.body["parts_count"])
	}
}

func TestReporter_TokenSentInHeaderOnlyNeverInBody(t *testing.T) {
	t.Setenv("CNC_STATE_DIR", t.TempDir())
	const token = "very-secret-bot-token"
	srv, reqs := captureServer(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"run":{"id":"run-token"},"linked":true}`))
	})

	fs := reportingSettings(srv.URL, "TEST_GMW_TOKEN_ENV_2")
	os.Setenv("TEST_GMW_TOKEN_ENV_2", token)
	defer os.Unsetenv("TEST_GMW_TOKEN_ENV_2")

	r := NewReporter(fs)
	st := New(fs, "mill-1")
	stop := startWatch(t, r, "mill-1", st)
	defer stop()

	st.emit(Event{Type: "status", Status: &Status{Running: true, JobID: "job-token", FilePath: "/x.nc", StartedAt: time.Now().UTC()}})

	req := waitReq(t, reqs)
	if req.token != token {
		t.Fatalf("X-Bot-Token = %q, want %q", req.token, token)
	}
	bodyJSON, _ := json.Marshal(req.body)
	if strings.Contains(string(bodyJSON), token) {
		t.Fatal("bot token leaked into the request body")
	}
}

// ── config / resilience ──────────────────────────────────────────────

func TestReporter_NoopWhenUnconfigured(t *testing.T) {
	t.Setenv("CNC_STATE_DIR", t.TempDir())
	fs := &fakeSettings{s: &settings.Settings{}} // Cnc.Reporting.GmwMesURL == ""
	r := NewReporter(fs)
	st := New(fs, "mill-1")
	stop := startWatch(t, r, "mill-1", st)
	defer stop()

	st.emit(Event{Type: "status", Status: &Status{Running: true, JobID: "job-5", FilePath: "/x.nc", StartedAt: time.Now().UTC()}})
	st.emit(Event{Type: "metric", Metric: &Metric{Key: "status_combined", Parsed: map[string]string{"program": "O0057", "parts": "1"}}})
	st.emit(Event{Type: "status", Status: &Status{Running: false, JobID: "job-5", FilePath: "/x.nc"}})

	// Give the goroutine a moment to have processed all three events,
	// then assert no run was ever tracked in memory or on disk.
	time.Sleep(100 * time.Millisecond)
	if run := r.getOpen("mill-1"); run != nil {
		t.Fatalf("expected no open run when unconfigured, got %+v", run)
	}
	if _, err := os.Stat(runMarkerPathFor("mill-1")); err == nil {
		t.Fatal("expected no run marker file when reporting is unconfigured")
	}
}

func TestReporter_RetryThenDropOn500(t *testing.T) {
	t.Setenv("CNC_STATE_DIR", t.TempDir())
	var mu sync.Mutex
	count := 0
	srv, _ := captureServer(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		mu.Lock()
		count++
		mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"detail":"boom"}`))
	})

	var logBuf strings.Builder
	prevOut := log.Writer()
	log.SetOutput(&logBuf)
	defer log.SetOutput(prevOut)

	fs := reportingSettings(srv.URL, "TEST_GMW_TOKEN_ENV")
	os.Setenv("TEST_GMW_TOKEN_ENV", "super-secret-token")
	defer os.Unsetenv("TEST_GMW_TOKEN_ENV")

	r := NewReporter(fs)
	st := New(fs, "mill-1")
	stop := startWatch(t, r, "mill-1", st)

	st.emit(Event{Type: "status", Status: &Status{Running: true, JobID: "job-6", FilePath: "/x.nc", StartedAt: time.Now().UTC()}})

	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := count
		mu.Unlock()
		if n >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected 2 attempts (1 try + 1 retry), got %d", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Give a beat for the drop log line to land, then stop.
	time.Sleep(50 * time.Millisecond)
	stop()

	mu.Lock()
	finalCount := count
	mu.Unlock()
	if finalCount != 2 {
		t.Fatalf("want exactly 2 attempts (no further retries), got %d", finalCount)
	}
	if run := r.getOpen("mill-1"); run == nil || run.RunID != "" {
		t.Fatalf("run_id should never have been set after a persistent 500, got %+v", run)
	}
	logged := logBuf.String()
	if !regexp.MustCompile(`(?i)dropping`).MatchString(logged) {
		t.Errorf("expected a drop log line, got: %s", logged)
	}
	if regexp.MustCompile(`super-secret-token`).MatchString(logged) {
		t.Fatal("bot token leaked into a log line")
	}
}

func TestReporter_RestartRecoveryClosesOpenRun(t *testing.T) {
	t.Setenv("CNC_STATE_DIR", t.TempDir())
	srv, reqs := captureServer(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"run":{},"carbon":{"attempted":false}}`))
	})

	// Simulate a previous process that opened a run and never closed
	// it before the daemon restarted.
	if err := writeOpenRunMarker("mill-1", &openRun{
		RunID: "run-orphan", MachineID: "mill-1", LastParts: 9, StartedAt: time.Now().Add(-time.Hour).UTC(),
	}); err != nil {
		t.Fatalf("seed marker: %v", err)
	}

	fs := reportingSettings(srv.URL, "")
	r := NewReporter(fs)
	st := New(fs, "mill-1")
	stop := startWatch(t, r, "mill-1", st)
	defer stop()

	req := waitReq(t, reqs)
	if req.path != "/api/machine/runs/run-orphan" || req.method != http.MethodPatch {
		t.Fatalf("want the recovery close to PATCH run-orphan, got %s %s", req.method, req.path)
	}
	if req.body["outcome"] != "unknown" {
		t.Errorf("outcome = %v, want unknown", req.body["outcome"])
	}
	if req.body["parts_count"] != float64(9) {
		t.Errorf("parts_count = %v, want 9 (carried over from the marker)", req.body["parts_count"])
	}
	if _, err := os.Stat(runMarkerPathFor("mill-1")); err == nil {
		t.Fatal("expected the run marker to be cleared after recovery")
	}
}
