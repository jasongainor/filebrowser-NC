package cnc

// Link owns the single, long-lived connection to one machine — either a
// TCP session to its Waveshare RS-232↔TCP bridge, or (see
// cnc/transport.go, cnc/serial_transport.go) a direct serial connection
// when Machine.Serial.Device is set. Everything below was written for
// the TCP case and still applies verbatim to serial: "connection" and
// "socket" mean whichever Conn buildTransport handed back.
//
// Why this exists: the bridge serves exactly ONE TCP client at a time,
// and the RS-232 side behind it has finite bandwidth. The original
// design treated it as a stateless request/response service — every
// Q-code poll dialled a fresh connection, ran one round-trip and closed
// (see the retired transientQuery path). With 17 metric pollers that is
// ~5 TCP connects/second against a ceiling of ~6.67 round-trips/second,
// which is why polling had to be duty-cycled behind an operator "wake
// window" and why responses periodically bled across connection
// boundaries (mode==program, G54 X==Y==Z).
//
// The Link inverts that: one goroutine owns one socket for its whole
// lifetime and every consumer — metric pollers, the tool-list/display
// endpoints, /api/cnc/qcode, and streaming jobs — funnels through it.
// Three properties fall out for free:
//
//   - No connection churn. One TCP session instead of thousands/hour.
//   - Response bleed is structurally impossible: a single reader on a
//     single socket cannot surface a late response on a *later*
//     connection. validateResponseShape stays as belt-and-braces.
//   - Honest liveness. lastGood tracks the last successful round-trip,
//     so "connected" means "the controller answered recently" instead
//     of "a human touched the dashboard in the last 5 minutes".
//
// Because serve() is single-goroutine, the old queryMu / lastQueryAt
// mutex dance is gone — spacing is just local state.

import (
	"bufio"
	"context"
	"errors"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"github.com/filebrowser/filebrowser/v2/settings"
)

const (
	// linkDialTimeout bounds one dial attempt at the bridge. Matches the
	// timeout the pre-Link streaming path used for its job dial.
	linkDialTimeout = 5 * time.Second

	// linkRetryFloor / linkRetryCeiling bound the reconnect backoff. The Link
	// redials forever — a bridge that is unplugged at lunch must come
	// back on its own without an operator poking anything. Jittered so a
	// multi-machine shop doesn't resynchronise its retries after a
	// switch reboot.
	linkRetryFloor = 1 * time.Second
	linkRetryCeiling = 30 * time.Second

	// defaultBaselineInterval is the fallback cadence for the always-on
	// liveness poll when settings.Cnc.BaselinePollSeconds is unset. One
	// round-trip per 15 s is ~1.5% of the bridge's ~6.67/s ceiling — low
	// enough to leave the RS-232 link effectively idle, frequent enough
	// that `connected` is meaningful to a display polling every 100 min.
	// Operator-tunable; see settings.Cnc.BaselinePollSeconds.
	defaultBaselineInterval = 15 * time.Second
)

// ErrLinkDown is returned to callers that submit work while the Link
// has no connection to the bridge. Carries the underlying dial/serve
// error as its message where one is known.
var ErrLinkDown = errors.New("bridge link is down")

// linkReq is one Q-code round-trip submitted to the Link's inbox.
type linkReq struct {
	q      int
	macroV *int
	ctx    context.Context
	respCh chan *QueryResult
}

// jobReq hands a streaming job's body to the Link so it can run on the
// Link's connection. body receives the live socket plus a pump()
// callback it must call periodically (between G-code lines) to service
// queued Q-code queries — that is what keeps /api/cnc/qcode and the
// dashboard responsive during a send.
type jobReq struct {
	ctx    context.Context
	body   func(conn Conn, br *bufio.Reader, pump func(), fc flowContext) error
	respCh chan error
}

// Link is the per-machine connection owner. Construct with NewLink and
// drive with Start/Stop; everything else is safe for concurrent use.
type Link struct {
	settings  settingsReader
	machineID string
	logf      func(level, format string, args ...any)

	reqCh chan *linkReq
	jobCh chan *jobReq

	mu        sync.Mutex
	up        bool
	jobActive bool
	lastGood  time.Time
	lastErr   string
	addr      string

	// flowMode / flowGate describe THIS connection's flow control and
	// are rebuilt on every successful dial in supervise() — a redial
	// starts unpaused, matching a freshly opened tty. flowMode is ""
	// for TCP and for serial configured with FlowControl "none" or
	// "rtscts" (rtscts is handled by polling CTS directly in
	// streamFile, not through flowGate). flowGate is non-nil only when
	// flowMode == "xonxoff". Both are read-only for the duration of one
	// serve() call, which runs on the same goroutine that set them
	// (supervise calls serve synchronously) — no mutex needed, same
	// reasoning as serve()'s local lastQueryAt.
	flowMode string
	flowGate *flowGate

	cancel  context.CancelFunc
	wg      sync.WaitGroup
	started bool
}

// NewLink builds a Link for one machine. logf may be nil.
func NewLink(s settingsReader, machineID string, logf func(level, format string, args ...any)) *Link {
	if logf == nil {
		logf = func(string, string, ...any) {}
	}
	return &Link{
		settings:  s,
		machineID: machineID,
		logf:      logf,
		// Depth mirrors the retired queryQueueDepth: the dashboard polls
		// one Q-code at a time and the pump drains one per G-code line,
		// so a deep buffer would only add latency to stale requests.
		reqCh: make(chan *linkReq, queryQueueDepth),
		jobCh: make(chan *jobReq),
	}
}

// Start launches the supervisor goroutine. Idempotent.
func (l *Link) Start(parent context.Context) {
	l.mu.Lock()
	if l.started {
		l.mu.Unlock()
		return
	}
	l.started = true
	ctx, cancel := context.WithCancel(parent)
	l.cancel = cancel
	l.mu.Unlock()

	l.wg.Add(1)
	go l.supervise(ctx)
}

// Stop tears the supervisor down and blocks until it exits.
func (l *Link) Stop() {
	l.mu.Lock()
	cancel := l.cancel
	l.cancel = nil
	l.started = false
	l.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	l.wg.Wait()
}

// baselineInterval resolves the operator-tuned always-on poll cadence,
// falling back to defaultBaselineInterval. Read live so a settings edit
// takes effect without a restart.
func (l *Link) baselineInterval() time.Duration {
	set, err := l.settings.Get()
	if err != nil {
		return defaultBaselineInterval
	}
	if n := set.Cnc.BaselinePollSeconds; n > 0 {
		return time.Duration(n) * time.Second
	}
	return defaultBaselineInterval
}

// Alive reports whether the controller is actually answering.
//
// True when a streaming job is in flight (we are pushing bytes at the
// controller right now) or when the last successful round-trip is
// within 3x the baseline poll interval. The 3x staleness multiplier
// mirrors Aggregator.Snapshot's existing heuristic.
//
// Note this is deliberately stricter than "the TCP dial succeeded": the
// Waveshare answers TCP even when the mill behind it is powered off, so
// dial success alone would report a dead machine as connected.
func (l *Link) Alive() bool {
	// Resolve the window BEFORE taking the mutex — settings.Get reaches
	// into storage and must not run under l.mu.
	window := 3 * l.baselineInterval()

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.jobActive {
		return true
	}
	if !l.up || l.lastGood.IsZero() {
		return false
	}
	return time.Since(l.lastGood) < window
}

// LinkState is the diagnostic snapshot surfaced on /api/cnc/status so an
// operator can see the link's health without reading logs.
type LinkState struct {
	Up       bool      `json:"up"`
	Alive    bool      `json:"alive"`
	Addr     string    `json:"addr,omitempty"`
	LastGood time.Time `json:"last_good,omitempty"`
	LastErr  string    `json:"last_error,omitempty"`
}

// State returns a point-in-time view of the link.
func (l *Link) State() LinkState {
	alive := l.Alive()
	l.mu.Lock()
	defer l.mu.Unlock()
	return LinkState{
		Up:       l.up,
		Alive:    alive,
		Addr:     l.addr,
		LastGood: l.lastGood,
		LastErr:  l.lastErr,
	}
}

// Query submits one Q-code round-trip and waits for the answer. Fails
// fast with ErrLinkDown when the bridge is unreachable rather than
// blocking the caller until its context expires.
func (l *Link) Query(ctx context.Context, qCode int, macroVar *int) (*QueryResult, error) {
	req := &linkReq{q: qCode, macroV: macroVar, ctx: ctx, respCh: make(chan *QueryResult, 1)}
	select {
	case l.reqCh <- req:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case res := <-req.respCh:
		return res, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// RunJob hands a streaming job body to the Link, which runs it on the
// live connection. Blocks until the body returns. Returns ErrLinkDown
// if the bridge is not connected — a job must never start against a
// dead link.
func (l *Link) RunJob(ctx context.Context, body func(conn Conn, br *bufio.Reader, pump func(), fc flowContext) error) error {
	req := &jobReq{ctx: ctx, body: body, respCh: make(chan error, 1)}
	select {
	case l.jobCh <- req:
	case <-ctx.Done():
		// Never handed off — the body did not run, so nothing reached
		// the controller.
		return ctx.Err()
	}
	// Deliberately NOT selecting on ctx.Done() here. Once the body owns
	// the socket we must wait for it to actually finish: returning early
	// on cancellation would let Stop() report the job over — and free
	// the streamer's job slot — while streamFile was still writing
	// G-code lines to the mill. The body honours ctx itself (streamFile
	// checks it every line), so this resolves promptly on Stop.
	return <-req.respCh
}

// ── supervisor ────────────────────────────────────────────────────────

// supervise is the reconnect loop: resolve → dial → serve → backoff →
// repeat, until the context is cancelled. While disconnected it keeps
// draining the inboxes with fast failures so callers never hang.
func (l *Link) supervise(ctx context.Context) {
	defer l.wg.Done()
	backoff := linkRetryFloor

	for {
		if ctx.Err() != nil {
			return
		}

		m, port, err := l.resolveMachine()
		if err != nil {
			l.markDown(err)
			if !l.rejectFor(ctx, backoff) {
				return
			}
			backoff = nextBackoff(backoff)
			continue
		}

		transport, err := buildTransport(m, port)
		if err != nil {
			l.markDown(err)
			if !l.rejectFor(ctx, backoff) {
				return
			}
			backoff = nextBackoff(backoff)
			continue
		}

		conn, err := transport.Dial(linkDialTimeout)
		if err != nil {
			l.markDown(err)
			if !l.rejectFor(ctx, backoff) {
				return
			}
			backoff = nextBackoff(backoff)
			continue
		}

		addr := transport.Addr()
		l.markUp(addr)
		// Rebuilt per connection — see the flowMode/flowGate field
		// comment. A TCP transport (or serial with FlowControl "none"
		// / "rtscts") reports "" here, so flowGate stays nil and every
		// XON/XOFF-aware call site below is a no-op for those.
		l.flowMode = transport.FlowControl()
		if l.flowMode == "xonxoff" {
			l.flowGate = newFlowGate()
		} else {
			l.flowGate = nil
		}
		l.logf("info", "bridge link established: %s", addr)
		backoff = linkRetryFloor

		serveErr := l.serve(ctx, conn)
		_ = conn.Close()

		if ctx.Err() != nil {
			l.markDown(context.Canceled)
			return
		}
		l.markDown(serveErr)
		if serveErr != nil {
			l.logf("warn", "bridge link lost (%v); reconnecting", serveErr)
		}
		if !l.rejectFor(ctx, backoff) {
			return
		}
		backoff = nextBackoff(backoff)
	}
}

// resolveMachine looks up this Link's machine in live settings, so an
// operator editing host/port takes effect on the next reconnect rather
// than requiring a restart.
func (l *Link) resolveMachine() (settings.Machine, int, error) {
	set, err := l.settings.Get()
	if err != nil {
		return settings.Machine{}, 0, err
	}
	m, ok := set.Cnc.MachineByID(l.machineID)
	if !ok || (m.Host == "" && strings.TrimSpace(m.Serial.Device) == "") {
		return settings.Machine{}, 0, ErrConfigMissing
	}
	port := m.Port
	if port == 0 {
		port = settings.DefaultHaasPort
	}
	return m, port, nil
}

// serve owns the connection. Single goroutine, so query spacing and the
// buffered reader need no synchronisation. Returns the error that cost
// us the connection (nil on context cancellation).
func (l *Link) serve(ctx context.Context, conn Conn) error {
	// ONE reader for the connection's lifetime. Allocating a fresh
	// bufio.Reader per exchange (as the transient path did) discards
	// whatever it had buffered beyond the current frame — harmless when
	// the socket is about to close, silent data loss when it is not.
	br := bufio.NewReader(conn)
	var lastQueryAt time.Time

	pump := func() {
		select {
		case req := <-l.reqCh:
			_ = l.service(conn, br, req, &lastQueryAt)
		default:
		}
	}

	for {
		select {
		case <-ctx.Done():
			return nil

		case req := <-l.reqCh:
			if err := l.service(conn, br, req, &lastQueryAt); err != nil {
				return err
			}

		case jr := <-l.jobCh:
			l.setJobActive(true)
			err := jr.body(conn, br, pump, flowContext{Mode: l.flowMode, Gate: l.flowGate})
			l.setJobActive(false)
			jr.respCh <- err
			if err != nil {
				// A job body error means the socket is suspect — drop it
				// and redial rather than reusing a connection whose state
				// we can no longer reason about.
				return err
			}
			l.noteGood()
		}
	}
}

// service runs one query on the live connection, applying the RS-232
// min-spacing floor. Returns a non-nil error only when the failure
// implicates the connection itself (so the supervisor redials);
// protocol-level failures come back on the request's response channel.
func (l *Link) service(conn Conn, br *bufio.Reader, req *linkReq, lastQueryAt *time.Time) error {
	if err := req.ctx.Err(); err != nil {
		req.respCh <- &QueryResult{Q: req.q, Var: req.macroV, Error: err.Error()}
		return nil
	}

	if !lastQueryAt.IsZero() {
		if wait := minQuerySpacing - time.Since(*lastQueryAt); wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-timer.C:
			case <-req.ctx.Done():
				timer.Stop()
				req.respCh <- &QueryResult{Q: req.q, Var: req.macroV, Error: req.ctx.Err().Error()}
				return nil
			}
		}
	}

	t0 := time.Now()
	raw, err := exchangeOnReader(conn, br, req.q, req.macroV, l.flowGate)
	*lastQueryAt = time.Now()

	res := &QueryResult{Q: req.q, Var: req.macroV, DurationMs: sinceMs(t0)}
	if err != nil {
		res.Error = err.Error()
		req.respCh <- res
		l.noteErr(err)

		if errors.Is(err, errNoResponse) {
			// Controller is silent but the socket is healthy (mill powered
			// off, Setting 143 off). Keep the connection — see errNoResponse.
			//
			// Resync first: if a late reply lands after we gave up it would
			// otherwise be read as the NEXT query's response. That is the
			// cross-talk class validateResponseShape exists to catch, and on
			// a persistent socket a stale frame would linger indefinitely
			// rather than dying with the connection.
			l.drain(conn, br)
			return nil
		}
		// Write error / EOF / reset — the socket itself is gone.
		return err
	}

	res.Raw = raw
	v := stripEchoAndFraming(raw)
	if err := validateResponseShape(req.q, req.macroV, v); err != nil {
		// Keep Raw for postmortems but withhold Value/Parsed so the UI
		// can't render a contaminated frame as truth. Not a connection
		// fault — the socket is fine, the frame was not.
		res.Error = err.Error()
		req.respCh <- res
		return nil
	}
	res.Value = v
	res.Parsed = parseValue(v, req.q, req.macroV)
	res.OK = true
	req.respCh <- res
	l.noteGood()
	return nil
}

// drain discards anything currently readable so the next exchange
// starts from a clean stream. Used after a timed-out query, where a
// late reply would otherwise be mistaken for the next query's answer.
//
// Bounded by a very short deadline: we only want bytes already in
// flight, not to wait for new ones.
func (l *Link) drain(conn Conn, br *bufio.Reader) {
	_ = conn.SetReadDeadline(time.Now().Add(dprntScavengeDeadline))
	defer func() { _ = conn.SetReadDeadline(time.Time{}) }()
	discarded := 0
	buf := make([]byte, 512)
	for {
		n, err := br.Read(buf)
		discarded += n
		if err != nil || n == 0 {
			break
		}
	}
	if discarded > 0 {
		l.logf("warn", "resynced bridge stream after a silent query (%d stale bytes discarded)", discarded)
	}
}

// rejectFor drains both inboxes for d, answering everything with the
// current link error so callers fail fast instead of blocking. Returns
// false when the context was cancelled (caller should exit).
func (l *Link) rejectFor(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(jitter(d))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
			return true
		case req := <-l.reqCh:
			req.respCh <- &QueryResult{Q: req.q, Var: req.macroV, Error: l.downReason()}
		case jr := <-l.jobCh:
			jr.respCh <- ErrLinkDown
		}
	}
}

func (l *Link) downReason() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.lastErr != "" {
		return l.lastErr
	}
	return ErrLinkDown.Error()
}

func (l *Link) markUp(addr string) {
	l.mu.Lock()
	l.up = true
	l.addr = addr
	l.lastErr = ""
	l.mu.Unlock()
}

func (l *Link) markDown(err error) {
	l.mu.Lock()
	l.up = false
	if err != nil {
		l.lastErr = err.Error()
	}
	l.mu.Unlock()
}

func (l *Link) noteGood() {
	l.mu.Lock()
	l.lastGood = time.Now()
	l.lastErr = ""
	l.mu.Unlock()
}

func (l *Link) noteErr(err error) {
	l.mu.Lock()
	if err != nil {
		l.lastErr = err.Error()
	}
	l.mu.Unlock()
}

func (l *Link) setJobActive(v bool) {
	l.mu.Lock()
	l.jobActive = v
	if v {
		l.lastGood = time.Now()
	}
	l.mu.Unlock()
}

// nextBackoff doubles toward linkRetryCeiling.
func nextBackoff(d time.Duration) time.Duration {
	d *= 2
	if d > linkRetryCeiling {
		return linkRetryCeiling
	}
	return d
}

// jitter spreads retries by ±25% so multiple machines don't resynchronise.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	delta := int64(d) / 2
	return time.Duration(int64(d)*3/4 + rand.Int64N(delta+1))
}
