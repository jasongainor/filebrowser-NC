package cnc

// Reporter — machine-run lifecycle reporting to gmw-mes, so machine
// time reaches Carbon (see the gmw-mes repo's docs/machine-runs.md and
// this repo's docs/RUN_REPORTING.md). Subscribes to a machine's event
// feed the same way registry.go's watchQueueAutoMatch does for queue
// auto-match and NewNotifier's callers do for Discord — running↑
// opens a run, a parts-count change / HaasLastError / log-level error
// posts an event, running↓ closes it.
//
// No-op end to end while settings.Cnc.Reporting.GmwMesURL is empty:
// Watch still subscribes (an idle channel costs nothing) but every
// hook checks config() first and returns immediately, so an
// unconfigured install never opens a socket or touches CNC_STATE_DIR
// for a run marker.
//
// Resilience: each machine gets its own bounded queue (reportQueueDepth)
// drained by a single worker goroutine, so requests for one run are
// always sent in the order they were generated (create before its
// events before its close) without a background HTTP call ever
// blocking the streamer's own event loop. A request gets one retry on
// a network error or 5xx, then is logged and dropped — a report that
// misses gmw-mes is gone, same trade cnc/notify.go makes for Discord.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/filebrowser/filebrowser/v2/settings"
)

const (
	// reportHTTPTimeout bounds a single POST/PATCH to gmw-mes.
	reportHTTPTimeout = 5 * time.Second
	// reportRetryDelay is the backoff before the one retry on a
	// network error or 5xx.
	reportRetryDelay = 300 * time.Millisecond
	// reportQueueDepth caps how many pending reports one machine can
	// queue up behind a slow/unreachable gmw-mes before new ones are
	// dropped (logged, never blocked).
	reportQueueDepth = 100
)

// reportTask is one unit of work on a machine's report queue. Always
// invoked from that machine's single worker goroutine, so tasks for
// the same run execute in the order they were enqueued.
type reportTask func(ctx context.Context)

// openRun is the in-memory (and, via the run marker file, persisted)
// record of one in-flight machine.run. Mirrors recovery.go's
// activeJobMarker in spirit: JSON, one file per machine, read back on
// the next Watch call so a daemon restart can still close a run that
// was open when it went down.
type openRun struct {
	RunID         string `json:"run_id"`
	JobID         string `json:"job_id,omitempty"` // streamer job id; ties this run back to job_history.go's outcome on close. Empty for an attach-only run.
	MachineID     string `json:"machine_id"`
	JobReadableID string `json:"job_readable_id,omitempty"`
	OperationRef  string `json:"operation_ref,omitempty"`
	PartID        string `json:"part_id,omitempty"`
	ProgramSHA256 string `json:"program_sha256,omitempty"`
	ONumber       string `json:"o_number,omitempty"`
	FileName      string `json:"file_name,omitempty"`
	// Kind classifies the run (cnc.RunKindTrial/Production/Rnd),
	// empty when nobody's said — gmw-mes's own default rule then
	// applies. Set at open time from the program's GMW-KIND header
	// (ParseIdentity), and/or later by an operator override via
	// SetKind, which also PATCHes it to gmw-mes immediately so the
	// eventual close carries the same value even if that PATCH is
	// dropped. Persisted in the run marker so a daemon restart
	// doesn't lose an override made just before it went down.
	Kind         string    `json:"kind,omitempty"`
	StartedAt    time.Time `json:"started_at"`
	LastParts    int       `json:"last_parts,omitempty"`
	HadError     bool      `json:"had_error,omitempty"`
	LastErrorMsg string    `json:"last_error_msg,omitempty"`
}

// Reporter posts program-run lifecycle events to gmw-mes. One
// instance is shared by the whole Registry; Watch is spun up per
// machine alongside watchQueueAutoMatch — see registry.go.
type Reporter struct {
	settings settingsReader
	client   *http.Client

	mu      sync.Mutex
	open    map[string]*openRun // machineID -> in-flight run, absent when idle
	oNumber map[string]string   // machineID -> last-seen controller O-number

	qmu    sync.Mutex
	queues map[string]chan reportTask
	wg     sync.WaitGroup
}

// NewReporter builds a Reporter bound to the live settings reader, so
// every hook picks up a config change without a restart — same
// contract as NewNotifier.
func NewReporter(s settingsReader) *Reporter {
	return &Reporter{
		settings: s,
		client:   &http.Client{Timeout: reportHTTPTimeout},
		open:     map[string]*openRun{},
		oNumber:  map[string]string{},
		queues:   map[string]chan reportTask{},
	}
}

// config reads the live Reporting config. ok is false when reporting
// isn't wired up (GmwMesURL empty) — every hook bails out on !ok.
func (r *Reporter) config() (settings.ReportingConfig, bool) {
	if r == nil {
		return settings.ReportingConfig{}, false
	}
	set, err := r.settings.Get()
	if err != nil {
		return settings.ReportingConfig{}, false
	}
	cfg := set.Cnc.Reporting
	return cfg, cfg.Enabled()
}

// Wait blocks until every per-machine worker this Reporter has spun up
// has exited (their governing ctx must already be cancelled — this
// does not itself stop anything). Registry.Stop() doesn't call this;
// like cnc/notify.go's fire-and-forget Discord posts, an in-flight
// report is allowed to lapse at process shutdown. Exists so tests can
// wait for full queue drain before asserting on marker files / mock
// server state instead of racing background goroutines.
func (r *Reporter) Wait() { r.wg.Wait() }

// Watch subscribes to one machine's event feed and translates
// running↑ / parts-count change / HaasLastError / log-level error /
// running↓ into gmw-mes machine-runs calls. Runs until ctx is
// cancelled (the registry's bgCtx — see registry.go's Refresh).
func (r *Reporter) Watch(ctx context.Context, machineID string, st *Streamer) {
	ch := r.queueFor(machineID)
	r.wg.Add(1)
	go r.worker(ctx, machineID, ch)

	// Restart recovery: a run marker surviving from a previous process
	// means that run never got a close call in. Fire it off now, before
	// touching any new events, so gmw-mes doesn't carry it open forever.
	r.recoverOpenRun(machineID)

	feed := st.Subscribe()
	defer st.Unsubscribe(feed)

	var wasRunning bool
	var lastAttachedFile string
	var lastHaasError string

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-feed:
			if !ok {
				return
			}
			switch ev.Type {
			case "metric":
				r.handleMetric(machineID, ev.Metric)
			case "status":
				if ev.Status == nil {
					continue
				}
				r.handleStatus(machineID, ev.Status, &wasRunning, &lastAttachedFile)
				if ev.Status.HaasLastError != "" && ev.Status.HaasLastError != lastHaasError {
					r.reportAlarm(machineID, ev.Status.HaasLastError)
				}
				lastHaasError = ev.Status.HaasLastError
			case "log":
				if ev.Level == "error" {
					r.reportError(machineID, ev.Msg)
				}
			}
		}
	}
}

// handleStatus drives the open/close state machine. "running↑" is
// either a real streaming job starting, or the daemon attaching to a
// program the controller is already running (SD card / USB / Ethernet
// drop — see streamer.go's Attach/AttachAuto); "running↓" is the
// mirror of whichever one opened the run.
func (r *Reporter) handleStatus(machineID string, status *Status, wasRunning *bool, lastAttachedFile *string) {
	switch {
	case status.Running && !*wasRunning:
		r.openRunFromJob(machineID, status)
	case !status.Running && *wasRunning:
		r.closeRun(machineID)
	case !status.Running && status.AttachedFile != "" && status.AttachedFile != *lastAttachedFile:
		r.openRunFromAttach(machineID, status)
	case !status.Running && status.AttachedFile == "" && *lastAttachedFile != "":
		r.closeRun(machineID)
	}
	*wasRunning = status.Running
	*lastAttachedFile = status.AttachedFile
}

// handleMetric watches the status_combined (Q500) aggregator metric
// for the controller's current O-number (cached for the next run open
// regardless of reporting being enabled — cheap, and keeps the cache
// warm) and its PARTS field, posting a "parts" event on change.
func (r *Reporter) handleMetric(machineID string, m *Metric) {
	if m == nil || m.Key != "status_combined" {
		return
	}
	program, parts, hasParts := parseStatusCombinedMetric(m)
	if program != "" {
		r.setONumber(machineID, program)
	}
	if _, ok := r.config(); !ok || !hasParts {
		return
	}
	run := r.getOpen(machineID)
	if run == nil {
		return
	}
	r.mu.Lock()
	changed := run.LastParts != parts
	run.LastParts = parts
	r.mu.Unlock()
	if !changed {
		return
	}
	r.persistOpen(machineID, run)
	r.enqueue(machineID, func(ctx context.Context) {
		r.doEvent(ctx, machineID, run, "parts", map[string]any{"parts_count": parts})
	})
}

// parseStatusCombinedMetric pulls the O-number and PARTS count out of
// a status_combined Metric. qcode.go's parseValue returns
// map[string]string for a Q500 frame; map[string]any is handled too
// so a hand-built Metric (tests, or a future parser change) still
// works.
func parseStatusCombinedMetric(m *Metric) (program string, parts int, hasParts bool) {
	switch parsed := m.Parsed.(type) {
	case map[string]string:
		program = parsed["program"]
		if v, ok := parsed["parts"]; ok {
			if n, err := strconv.Atoi(v); err == nil {
				parts, hasParts = n, true
			}
		}
	case map[string]any:
		if v, ok := parsed["program"].(string); ok {
			program = v
		}
		if v, ok := parsed["parts"]; ok {
			switch vv := v.(type) {
			case string:
				if n, err := strconv.Atoi(vv); err == nil {
					parts, hasParts = n, true
				}
			case float64:
				parts, hasParts = int(vv), true
			case int:
				parts, hasParts = vv, true
			}
		}
	}
	return program, parts, hasParts
}

// openRunFromJob opens a run for a real streaming job. Identity comes
// from ParseIdentity of the file the streamer is actually sending —
// found via the active-job marker's AbsPath (recovery.go), which
// Start() writes before this "running" status event ever fires. A
// program with no GMW header (ParseIdentity returns found=false)
// degrades gracefully: the run is still opened and reported, just
// without job_readable_id/operation_ref/part_id/program_sha256 — same
// rule the gmw-mes side documents for a missing GMW-JOB/GMW-PART.
func (r *Reporter) openRunFromJob(machineID string, status *Status) {
	cfg, ok := r.config()
	if !ok {
		return
	}
	r.closeRun(machineID) // supersede any stale open run — shouldn't normally happen

	run := &openRun{
		JobID:     status.JobID,
		MachineID: cfg.GmwMesMachineID(machineID),
		StartedAt: status.StartedAt,
		FileName:  filepath.Base(status.FilePath),
		ONumber:   r.currentONumber(machineID),
	}
	if run.StartedAt.IsZero() {
		run.StartedAt = time.Now().UTC()
	}

	if marker := readMarkerFor(machineID); marker != nil && marker.JobID == status.JobID && marker.AbsPath != "" {
		if nc, err := os.ReadFile(marker.AbsPath); err == nil {
			if id, found := ParseIdentity(nc); found {
				run.JobReadableID = id.Job
				run.OperationRef = id.Operation
				run.PartID = id.Part
				run.Kind = id.Kind
				if id.ComputedSHA256 != "" {
					run.ProgramSHA256 = strings.ToLower(id.ComputedSHA256)
				}
			}
		}
	}

	r.setOpen(machineID, run)
	r.persistOpen(machineID, run)
	r.enqueue(machineID, func(ctx context.Context) {
		r.doCreate(ctx, machineID, run)
	})
}

// openRunFromAttach opens a run for a program the controller is
// already running that the daemon merely attached to (no absolute
// path is available for an attach — see streamer.go's doc comment on
// AttachedFile being share-relative — so this run carries only the
// O-number and file name, no GMW-* identity).
func (r *Reporter) openRunFromAttach(machineID string, status *Status) {
	cfg, ok := r.config()
	if !ok {
		return
	}
	r.closeRun(machineID)

	run := &openRun{
		MachineID: cfg.GmwMesMachineID(machineID),
		StartedAt: status.AttachedAt,
		FileName:  filepath.Base(status.AttachedFile),
		ONumber:   r.currentONumber(machineID),
	}
	if run.StartedAt.IsZero() {
		run.StartedAt = time.Now().UTC()
	}

	r.setOpen(machineID, run)
	r.persistOpen(machineID, run)
	r.enqueue(machineID, func(ctx context.Context) {
		r.doCreate(ctx, machineID, run)
	})
}

// closeRun closes machineID's open run, if any. Outcome comes from
// job_history.go's own classification (completed/stopped/error — see
// recordJobHistory), which by the time this fires has already been
// appended by the streamer's run() defer, for a real job; an
// attach-only run (no JobID) has no history row, so it falls back to
// "error" when the last thing reported was a log-level error, else
// "unknown" — gmw-mes accepts "unknown" alongside the daemon's own
// outcome vocabulary (docs/machine-runs.md).
func (r *Reporter) closeRun(machineID string) {
	if _, ok := r.config(); !ok {
		return
	}
	run := r.takeOpen(machineID)
	if run == nil {
		return
	}
	clearOpenRunMarker(machineID)

	snap := r.snapshot(run)
	outcome, errMsg := "unknown", ""
	if snap.JobID != "" {
		if entries, err := ReadJobHistory(machineID, 5); err == nil {
			for _, e := range entries {
				if e.JobID == snap.JobID {
					outcome, errMsg = e.Status, e.ErrorMsg
					break
				}
			}
		}
	} else if snap.HadError {
		outcome, errMsg = "error", snap.LastErrorMsg
	}

	r.enqueue(machineID, func(ctx context.Context) {
		r.doClose(ctx, machineID, run, outcome, snap.LastParts, errMsg)
	})
}

// reportAlarm posts an "alarm" event for the machine's open run, if
// any. No-op (not an error) when nothing is open — HaasLastError can
// surface outside of a tracked run.
func (r *Reporter) reportAlarm(machineID, message string) {
	if _, ok := r.config(); !ok {
		return
	}
	run := r.getOpen(machineID)
	if run == nil {
		return
	}
	r.enqueue(machineID, func(ctx context.Context) {
		r.doEvent(ctx, machineID, run, "alarm", map[string]any{"message": message})
	})
}

// reportError posts an "error" event for the machine's open run and
// records HadError so a subsequent attach-only close (no job_history
// row to consult) reports outcome "error" instead of "unknown".
func (r *Reporter) reportError(machineID, message string) {
	if _, ok := r.config(); !ok {
		return
	}
	run := r.getOpen(machineID)
	if run == nil {
		return
	}
	r.mu.Lock()
	run.HadError = true
	run.LastErrorMsg = message
	r.mu.Unlock()
	r.persistOpen(machineID, run)
	r.enqueue(machineID, func(ctx context.Context) {
		r.doEvent(ctx, machineID, run, "error", map[string]any{"message": message})
	})
}

// SetKind overrides the kind (RunKindTrial/Production/Rnd — see
// program_identity.go) of machineID's currently open run: it takes
// effect in memory and in the run marker immediately, PATCHes it to
// gmw-mes right away, and is remembered so the run's eventual close
// PATCH carries the same value even if this immediate PATCH is
// dropped after retries. This is the daemon's side of the operator
// override described in docs/RUN_REPORTING.md — the cncd
// /api/cnc/run-kind route is the only caller. Returns false when no
// run is currently open for machineID (including when reporting is
// unconfigured, since Reporter tracks no open runs at all then) —
// the route turns that into a 409.
func (r *Reporter) SetKind(machineID, kind string) bool {
	run := r.getOpen(machineID)
	if run == nil {
		return false
	}
	r.mu.Lock()
	run.Kind = kind
	r.mu.Unlock()
	r.persistOpen(machineID, run)
	r.enqueue(machineID, func(ctx context.Context) {
		r.doSetKind(ctx, machineID, run, kind)
	})
	return true
}

// OpenRunInfo returns the kind and gmw-mes run_id of machineID's
// currently open run. ok is false when nothing is open — same "no
// run tracked while reporting is unconfigured" caveat as SetKind.
// Used by the /api/cnc/run-kind GET route and to add the run_kind/
// run_id fields to /api/cnc/state's snapshot.
func (r *Reporter) OpenRunInfo(machineID string) (kind, runID string, ok bool) {
	run := r.getOpen(machineID)
	if run == nil {
		return "", "", false
	}
	snap := r.snapshot(run)
	return snap.Kind, snap.RunID, true
}

// ── in-memory state helpers — all access to open/oNumber/openRun
// fields goes through these so Watch's goroutine and the per-machine
// worker goroutine never touch them unlocked. ──

func (r *Reporter) getOpen(machineID string) *openRun {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.open[machineID]
}

func (r *Reporter) setOpen(machineID string, run *openRun) {
	r.mu.Lock()
	r.open[machineID] = run
	r.mu.Unlock()
}

func (r *Reporter) takeOpen(machineID string) *openRun {
	r.mu.Lock()
	defer r.mu.Unlock()
	run := r.open[machineID]
	delete(r.open, machineID)
	return run
}

func (r *Reporter) snapshot(run *openRun) openRun {
	r.mu.Lock()
	defer r.mu.Unlock()
	return *run
}

func (r *Reporter) runID(run *openRun) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return run.RunID
}

func (r *Reporter) setRunID(run *openRun, id string) {
	r.mu.Lock()
	run.RunID = id
	r.mu.Unlock()
}

func (r *Reporter) currentONumber(machineID string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.oNumber[machineID]
}

func (r *Reporter) setONumber(machineID, v string) {
	r.mu.Lock()
	r.oNumber[machineID] = v
	r.mu.Unlock()
}

// persistOpen writes run's current state to the machine's run marker
// so a restart can recover it. Best-effort — a write failure is
// logged, never fatal to the run itself.
func (r *Reporter) persistOpen(machineID string, run *openRun) {
	snap := r.snapshot(run)
	if err := writeOpenRunMarker(machineID, &snap); err != nil {
		log.Printf("[cnc:reporter:%s] persist run marker: %v", machineID, err)
	}
}

// recoverOpenRun is called once at the start of Watch. A marker file
// surviving from a previous process means that run's close never made
// it to gmw-mes — close it now with outcome "unknown" rather than
// leaving it open forever. Clears the marker either way (even when
// reporting is currently disabled) so a stale file doesn't linger.
func (r *Reporter) recoverOpenRun(machineID string) {
	run := readOpenRunMarker(machineID)
	if run == nil {
		return
	}
	clearOpenRunMarker(machineID)
	if _, ok := r.config(); !ok {
		return
	}
	r.enqueue(machineID, func(ctx context.Context) {
		r.doClose(ctx, machineID, run, "unknown", run.LastParts, "daemon restarted while this run was open")
	})
}

// ── run marker persistence — same directory + atomic-write pattern as
// recovery.go's active_job_<machine>.json, one file per machine so two
// machines' restarts don't collide. ──

func runMarkerPathFor(machineID string) string {
	if machineID == "" {
		machineID = "default"
	}
	return filepath.Join(markerStateDir(), "active_run_"+machineID+".json")
}

func writeOpenRunMarker(machineID string, run *openRun) error {
	p := runMarkerPathFor(machineID)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	buf, err := json.Marshal(run)
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func readOpenRunMarker(machineID string) *openRun {
	buf, err := os.ReadFile(runMarkerPathFor(machineID))
	if err != nil {
		return nil
	}
	var run openRun
	if err := json.Unmarshal(buf, &run); err != nil {
		return nil
	}
	return &run
}

func clearOpenRunMarker(machineID string) {
	_ = os.Remove(runMarkerPathFor(machineID))
}

// ── per-machine queue + worker ──

func (r *Reporter) queueFor(machineID string) chan reportTask {
	r.qmu.Lock()
	defer r.qmu.Unlock()
	ch, ok := r.queues[machineID]
	if !ok {
		ch = make(chan reportTask, reportQueueDepth)
		r.queues[machineID] = ch
	}
	return ch
}

// enqueue queues task on machineID's worker. Drops (logging) rather
// than blocking when the queue is already at reportQueueDepth — a
// wedged gmw-mes must never back up into the streamer's event loop.
func (r *Reporter) enqueue(machineID string, task reportTask) {
	ch := r.queueFor(machineID)
	select {
	case ch <- task:
	default:
		log.Printf("[cnc:reporter:%s] report queue full (cap %d), dropping", machineID, reportQueueDepth)
	}
}

// worker drains one machine's queue strictly in order, so a run's
// create always lands before its events and its close — the ordering
// invariant every doCreate/doEvent/doClose caller above relies on.
// Exits when ctx (the registry's bgCtx) is cancelled.
func (r *Reporter) worker(ctx context.Context, machineID string, ch chan reportTask) {
	defer r.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case task, ok := <-ch:
			if !ok {
				return
			}
			task(ctx)
		}
	}
}

// ── gmw-mes HTTP calls ──

func (r *Reporter) doCreate(ctx context.Context, machineID string, run *openRun) {
	cfg, ok := r.config()
	if !ok {
		return
	}
	snap := r.snapshot(run)
	body := map[string]any{
		"machine_id": snap.MachineID,
		"started_at": snap.StartedAt.UTC().Format(time.RFC3339),
	}
	if snap.JobReadableID != "" {
		body["job_readable_id"] = snap.JobReadableID
	}
	if snap.OperationRef != "" {
		body["operation_ref"] = snap.OperationRef
	}
	if snap.PartID != "" {
		body["part_id"] = snap.PartID
	}
	if snap.ProgramSHA256 != "" {
		body["program_sha256"] = snap.ProgramSHA256
	}
	if snap.ONumber != "" {
		body["o_number"] = snap.ONumber
	}
	if snap.FileName != "" {
		body["file_name"] = snap.FileName
	}
	if snap.Kind != "" {
		body["kind"] = snap.Kind
	}

	respBody, _, err := r.send(ctx, cfg, http.MethodPost, "/api/machine/runs", body)
	if err != nil {
		r.logDrop(machineID, "create run", err)
		return
	}
	var out struct {
		Run struct {
			ID string `json:"id"`
		} `json:"run"`
	}
	if jsonErr := json.Unmarshal(respBody, &out); jsonErr != nil || out.Run.ID == "" {
		r.logDrop(machineID, "create run", fmt.Errorf("unexpected response shape from gmw-mes"))
		return
	}
	r.setRunID(run, out.Run.ID)
	r.persistOpen(machineID, run)
}

func (r *Reporter) doEvent(ctx context.Context, machineID string, run *openRun, eventType string, detail any) {
	cfg, ok := r.config()
	if !ok {
		return
	}
	runID := r.runID(run)
	if runID == "" {
		// create never got a run_id (dropped after retries, or still
		// in flight isn't possible here — the worker is serial) —
		// nothing to attach this event to.
		return
	}
	body := map[string]any{"type": eventType, "detail": detail}
	if _, _, err := r.send(ctx, cfg, http.MethodPost, "/api/machine/runs/"+runID+"/events", body); err != nil {
		r.logDrop(machineID, eventType+" event", err)
	}
}

// doSetKind is SetKind's queued half — an immediate best-effort PATCH
// of {kind} to the open run. A no-op (not an error) when create never
// got a run_id yet, same rule as doEvent; the run's own Kind field is
// already updated by SetKind regardless, so the eventual close PATCH
// carries it even when this one is dropped.
func (r *Reporter) doSetKind(ctx context.Context, machineID string, run *openRun, kind string) {
	cfg, ok := r.config()
	if !ok {
		return
	}
	runID := r.runID(run)
	if runID == "" {
		return
	}
	if _, _, err := r.send(ctx, cfg, http.MethodPatch, "/api/machine/runs/"+runID, map[string]any{"kind": kind}); err != nil {
		r.logDrop(machineID, "set kind", err)
	}
}

func (r *Reporter) doClose(ctx context.Context, machineID string, run *openRun, outcome string, parts int, errMsg string) {
	cfg, ok := r.config()
	if !ok {
		return
	}
	runID := r.runID(run)
	if runID == "" {
		return
	}
	body := map[string]any{
		"ended_at":    time.Now().UTC().Format(time.RFC3339),
		"outcome":     outcome,
		"parts_count": parts,
	}
	if errMsg != "" {
		body["error"] = errMsg
	}
	if kind := r.snapshot(run).Kind; kind != "" {
		body["kind"] = kind
	}
	if _, _, err := r.send(ctx, cfg, http.MethodPatch, "/api/machine/runs/"+runID, body); err != nil {
		r.logDrop(machineID, "close run", err)
	}
}

// send performs one gmw-mes request with a single retry on a network
// error or 5xx response (reportRetryDelay backoff), then gives up. A
// 4xx is not retried — it means the request itself is wrong, not that
// gmw-mes is having a bad moment. The bot token is set on the request
// header only; it never appears in the URL, the body, or any returned
// error string.
func (r *Reporter) send(ctx context.Context, cfg settings.ReportingConfig, method, path string, body any) ([]byte, int, error) {
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, 0, fmt.Errorf("gmw-mes %s %s: encode body: %w", method, path, err)
	}
	token := os.Getenv(cfg.TokenEnvName())
	url := strings.TrimRight(cfg.GmwMesURL, "/") + path

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, 0, ctx.Err()
			case <-time.After(reportRetryDelay):
			}
		}

		respBody, status, err := r.doOnce(ctx, method, url, token, buf)
		if err != nil {
			lastErr = fmt.Errorf("gmw-mes %s %s: %w", method, path, err)
			continue // network error — retry
		}
		if status >= 500 {
			lastErr = fmt.Errorf("gmw-mes %s %s: status %d", method, path, status)
			continue // server error — retry
		}
		if status >= 400 {
			return respBody, status, fmt.Errorf("gmw-mes %s %s: status %d: %s", method, path, status, string(respBody))
		}
		return respBody, status, nil
	}
	return nil, 0, lastErr
}

func (r *Reporter) doOnce(ctx context.Context, method, url, token string, buf []byte) ([]byte, int, error) {
	reqCtx, cancel := context.WithTimeout(ctx, reportHTTPTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, method, url, bytes.NewReader(buf))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("X-Bot-Token", token)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return respBody, resp.StatusCode, nil
}

func (r *Reporter) logDrop(machineID, what string, err error) {
	log.Printf("[cnc:reporter:%s] %s failed, dropping: %v", machineID, what, err)
}
