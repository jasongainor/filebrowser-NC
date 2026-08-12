package cnc

// State aggregator — background poller that keeps a current snapshot
// of curated Q-code metrics so /api/cnc/state returns instantly without
// dragging on the Haas's MIN_QUERY_SPACING (~150 ms per round-trip).
//
// Each metric runs on its own ticker so fast-changing fields (mode,
// spindle, position) refresh more often than slow-changing ones (G54
// offsets, total power-on time). All polls go through Streamer.Query
// so the existing single-flight + idle-vs-streaming routing applies.

import (
	"context"
	"math/rand/v2"
	"sync"
	"time"
)

// Metric is one polled Q-code field. interval governs how often a fresh
// query is dispatched. parsed is the structured value from parseValue.
type Metric struct {
	Key        string        `json:"key"`
	Label      string        `json:"label"`
	QCode      int           `json:"q_code"`
	MacroVar   *int          `json:"macro_var,omitempty"`
	Interval   time.Duration `json:"-"`
	IntervalS  float64       `json:"interval_s"`
	Raw        string        `json:"raw,omitempty"`
	Value      string        `json:"value,omitempty"`
	Parsed     any           `json:"parsed,omitempty"`
	LastUpdate time.Time     `json:"last_update,omitempty"`
	LastError  string        `json:"last_error,omitempty"`
	OK         bool          `json:"ok"`
	Stale      bool          `json:"stale"`

	// lastAttempt is when we last dispatched a query for this metric,
	// successful or not. Distinct from LastUpdate (last SUCCESS) because
	// the baseline rate limiter must throttle attempts even while the
	// controller is silent. Unexported: internal scheduling, not wire state.
	lastAttempt time.Time
}

// metricSpec describes the things we poll. Mirrors haas-dashboard's
// METRICS list so an external consumer using the same key shape can
// migrate from the dashboard's /api/state to this one.
type metricSpec struct {
	Key      string
	Label    string
	QCode    int
	MacroVar *int
	Interval time.Duration

	// Baseline marks a metric that keeps polling at the slower
	// BaselinePollSeconds cadence even when no operator is at the
	// dashboard. Keep this set TINY — it is the always-on load on a
	// single-client RS-232 bridge. Its real job is to keep the Link's
	// lastGood timestamp fresh so `connected` means something to
	// non-interactive consumers like the e-paper display.
	Baseline bool
}

func ptr[T any](v T) *T { return &v }

var defaultMetricSpecs = []metricSpec{
	// mode (Q104) is the baseline liveness probe: cheapest useful query,
	// no macro variable, and its success is what keeps the Link's
	// lastGood — and therefore `connected` — honest around the clock.
	{"mode", "Mode", 104, nil, 3 * time.Second, true},
	{"tool", "Current tool", 201, nil, 5 * time.Second, false},
	{"last_cycle", "Last cycle time", 303, nil, 10 * time.Second, false},
	{"parts", "Parts counter", 402, nil, 3 * time.Second, false},
	{"status_combined", "Program / status", 500, nil, 3 * time.Second, false},
	// #3030 is the read-only "block number being executed" Haas macro on
	// most Next Gen + classic firmware. Populated when running a program
	// (any source — MEM, DNC, SD card, ethernet drop), 0 / unset while
	// idle. Used by the dashboard to follow along on attached files
	// where there's no Streamer.line_current. Falls back to a position-
	// to-line heuristic on the frontend when this stays 0.
	{"current_block", "Current N-block", 600, ptr(3030), 1500 * time.Millisecond, false},
	{"spindle_actual", "Spindle RPM (actual)", 600, ptr(3027), 3 * time.Second, false},
	{"spindle_cmd", "Spindle RPM (commanded)", 600, ptr(1815), 5 * time.Second, false},
	{"pos_x", "Machine X", 600, ptr(5021), 2500 * time.Millisecond, false},
	{"pos_y", "Machine Y", 600, ptr(5022), 2500 * time.Millisecond, false},
	{"pos_z", "Machine Z", 600, ptr(5023), 2500 * time.Millisecond, false},
	{"work_x", "Work X", 600, ptr(5041), 2500 * time.Millisecond, false},
	{"work_y", "Work Y", 600, ptr(5042), 2500 * time.Millisecond, false},
	{"work_z", "Work Z", 600, ptr(5043), 2500 * time.Millisecond, false},
	{"g54_x", "G54 X", 600, ptr(5221), 30 * time.Second, false},
	{"g54_y", "G54 Y", 600, ptr(5222), 30 * time.Second, false},
	{"g54_z", "G54 Z", 600, ptr(5223), 30 * time.Second, false},
}

// defaultWakeWindow is how long an operator-initiated wake keeps the
// aggregator on its FULL metric set before falling back to the baseline
// tier. Long enough that a person genuinely working at /machine is
// never surprised by data going stale; short enough that a forgotten
// tab doesn't run the bridge at 75% saturation for hours. Each fresh
// /state, /check, or /start extends it.
//
// Note this window no longer has anything to do with connectivity —
// the Link stays connected either way, and `connected` comes from
// Link.Alive(). It governs polling RATE only.
const defaultWakeWindow = 5 * time.Minute

// Aggregator owns the background pollers and the shared snapshot.
type Aggregator struct {
	streamer *Streamer

	mu      sync.RWMutex
	metrics map[string]*Metric

	cancel context.CancelFunc
	wg     sync.WaitGroup

	// wakeMu guards wakeUntil. Separate from mu so a fast Wake() call
	// from a request handler never contends with a poller doing its
	// snapshot update.
	wakeMu    sync.Mutex
	wakeUntil time.Time
}

// Wake extends the polling window to now+d (or leaves it alone if a
// later wake is already in flight). Pass 0 to use defaultWakeWindow.
// Operators reach this through any route that implies they're looking
// at machine state — /api/cnc/state, /api/cnc/check, /api/cnc/start.
// WS subscribers do NOT wake; mounting the global header pill on every
// authenticated layout shouldn't drag the bridge into a polling loop.
//
// Streams are unaffected by this — Streamer.run() owns its own context.
func (a *Aggregator) Wake(d time.Duration) {
	if d <= 0 {
		d = defaultWakeWindow
	}
	until := time.Now().Add(d)
	a.wakeMu.Lock()
	if until.After(a.wakeUntil) {
		a.wakeUntil = until
	}
	a.wakeMu.Unlock()
}

// IsAwake returns true if the aggregator's wake window hasn't expired.
func (a *Aggregator) IsAwake() bool {
	a.wakeMu.Lock()
	defer a.wakeMu.Unlock()
	return time.Now().Before(a.wakeUntil)
}

// NewAggregator wires the streamer reference and seeds the metric map
// from defaultMetricSpecs. Call Start to kick off the pollers.
func NewAggregator(s *Streamer) *Aggregator {
	a := &Aggregator{
		streamer: s,
		metrics:  make(map[string]*Metric, len(defaultMetricSpecs)),
	}
	for _, spec := range defaultMetricSpecs {
		a.metrics[spec.Key] = &Metric{
			Key:       spec.Key,
			Label:     spec.Label,
			QCode:     spec.QCode,
			MacroVar:  spec.MacroVar,
			Interval:  spec.Interval,
			IntervalS: spec.Interval.Seconds(),
			OK:        true,
			Stale:     true,
		}
	}
	return a
}

// Start launches one goroutine per metric. Each runs at its own
// interval and updates the shared map under mu. Safe to call once;
// subsequent calls are no-ops.
func (a *Aggregator) Start(parent context.Context) {
	if a.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	a.cancel = cancel
	for _, spec := range defaultMetricSpecs {
		spec := spec
		a.wg.Add(1)
		go a.pollLoop(ctx, spec)
	}
}

// Stop cancels all pollers and blocks until they exit. Safe to call
// when Start was never invoked.
func (a *Aggregator) Stop() {
	if a.cancel == nil {
		return
	}
	a.cancel()
	a.wg.Wait()
	a.cancel = nil
}

func (a *Aggregator) pollLoop(ctx context.Context, spec metricSpec) {
	defer a.wg.Done()
	// Stagger the first tick by a random fraction of the interval so
	// we don't fire 16 queries on the same tick. Combined with the
	// streamer's queryMu serialization this means the initial poll
	// burst spreads naturally across the slowest interval window.
	jitter := time.Duration(rand.Int64N(int64(spec.Interval)))
	timer := time.NewTimer(jitter)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		a.pollOnce(ctx, spec)
		timer.Reset(spec.Interval)
	}
}

func (a *Aggregator) pollOnce(ctx context.Context, spec metricSpec) {
	// Don't fire if this Aggregator's Machine isn't configured —
	// Query would error every tick. resolveMachine handles ID lookup
	// + legacy fallback in one place.
	if _, _, err := a.streamer.resolveMachine(); err != nil {
		return
	}
	// Don't poll while a job is streaming. The streaming socket carries
	// G-code line traffic + flow control; Q-code responses interleaved
	// with that come out as garbage (program/mode fields rendering raw
	// G-code is the obvious symptom). Mark the metric stale so the UI
	// shows "—" instead of a frozen value, and skip the round-trip.
	if a.streamer.IsRunning() {
		a.mu.Lock()
		if m := a.metrics[spec.Key]; m != nil {
			m.Stale = true
		}
		a.mu.Unlock()
		return
	}
	// Rate tier. The wake window is now a RATE policy, not a connection
	// policy — the Link stays connected regardless. Two tiers:
	//
	//   engaged  (operator at the dashboard, wake window live) — every
	//            metric at its own Interval, as before.
	//   baseline (nobody watching) — only Baseline metrics, throttled to
	//            BaselinePollSeconds. This is what keeps `connected`
	//            truthful for the e-paper display, which polls once
	//            every ~100 minutes and would never coincide with a
	//            5-minute operator wake window.
	//
	// IMPORTANT: this skip ONLY governs metric polling. Active streams
	// are unaffected — the IsRunning() check above already returned, so
	// we never reach this branch during a job.
	if !a.IsAwake() {
		if !spec.Baseline {
			return
		}
		if a.throttledBaseline(spec) {
			return
		}
	}
	a.markAttempt(spec)

	queryCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	res, err := a.streamer.Query(queryCtx, spec.QCode, spec.MacroVar)

	a.mu.Lock()
	m := a.metrics[spec.Key]
	if m == nil {
		a.mu.Unlock()
		return
	}
	now := time.Now()
	if err != nil {
		m.OK = false
		m.LastError = err.Error()
		m.Stale = true
		// Snapshot under the lock for the broadcast outside it.
		snap := *m
		a.mu.Unlock()
		a.streamer.emit(Event{Type: "metric", Metric: &snap})
		return
	}

	// Cheap change detection — skip the broadcast if the value didn't
	// actually move. Saves WS chatter for slow-changing fields like
	// G54 offsets that emit "MACRO, 5221, 0.0000" every 30 s.
	changed := m.Value != res.Value || m.OK != res.OK || m.LastError != res.Error
	m.OK = res.OK
	m.Raw = res.Raw
	m.Value = res.Value
	m.Parsed = res.Parsed
	m.LastError = res.Error
	m.LastUpdate = now
	m.Stale = false
	snap := *m
	a.mu.Unlock()

	if changed {
		a.streamer.emit(Event{Type: "metric", Metric: &snap})
	}
}

// throttledBaseline reports whether this baseline metric has already
// been attempted within the baseline interval, so standby polling holds
// its slow cadence instead of the metric's engaged Interval.
func (a *Aggregator) throttledBaseline(spec metricSpec) bool {
	// Resolve outside the lock — it reads settings.
	window := a.streamer.Link().baselineInterval()
	a.mu.Lock()
	defer a.mu.Unlock()
	m := a.metrics[spec.Key]
	if m == nil || m.lastAttempt.IsZero() {
		return false
	}
	return time.Since(m.lastAttempt) < window
}

func (a *Aggregator) markAttempt(spec metricSpec) {
	a.mu.Lock()
	if m := a.metrics[spec.Key]; m != nil {
		m.lastAttempt = time.Now()
	}
	a.mu.Unlock()
}

// Snapshot returns a deep-enough copy of the current metric map for a
// JSON response. Marks any metric whose last_update is older than 3x
// its interval as stale (matches haas-dashboard's heuristic).
func (a *Aggregator) Snapshot() map[string]*Metric {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make(map[string]*Metric, len(a.metrics))
	now := time.Now()
	for k, m := range a.metrics {
		// Shallow copy is fine — the only ref-typed field is MacroVar
		// (a *int constant from defaultMetricSpecs) and Parsed
		// (immutable any). Aliasing is safe.
		c := *m
		if !m.LastUpdate.IsZero() {
			c.Stale = now.Sub(m.LastUpdate) > 3*m.Interval
		}
		out[k] = &c
	}
	return out
}
