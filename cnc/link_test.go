package cnc

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/filebrowser/filebrowser/v2/settings"
)

// fakeBridge is a stand-in for the Waveshare RS-232↔TCP gateway. Like
// the real device it serves ONE client at a time, and it counts accepts
// so tests can assert the Link is not churning connections.
type fakeBridge struct {
	ln net.Listener

	mu       sync.Mutex
	accepts  int
	queries  []string
	closeNow bool // when set, drop the connection right after the next reply
}

func newFakeBridge(t *testing.T) *fakeBridge {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	b := &fakeBridge{ln: ln}
	go b.serve()
	t.Cleanup(func() { _ = ln.Close() })
	return b
}

func (b *fakeBridge) serve() {
	for {
		conn, err := b.ln.Accept()
		if err != nil {
			return
		}
		b.mu.Lock()
		b.accepts++
		b.mu.Unlock()
		b.handle(conn)
	}
}

// handle answers queries on one connection until the peer goes away.
// Replies use the real STX…ETB framing so stripEchoAndFraming and
// validateResponseShape run for real.
func (b *fakeBridge) handle(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		q := strings.TrimSpace(line)
		b.mu.Lock()
		b.queries = append(b.queries, q)
		drop := b.closeNow
		b.mu.Unlock()

		if _, err := conn.Write([]byte(replyFor(q))); err != nil {
			return
		}
		if drop {
			return
		}
	}
}

// replyFor produces a plausible framed answer for the handful of
// Q-codes the tests use.
func replyFor(q string) string {
	switch {
	case strings.HasPrefix(q, "?Q104"):
		return "\x02MEM\x17\r\n>"
	case strings.HasPrefix(q, "?Q201"):
		return "\x02TOOL, 3\x17\r\n>"
	default:
		return "\x02PROGRAM, O00042, IDLE\x17\r\n>"
	}
}

func (b *fakeBridge) acceptCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.accepts
}

func (b *fakeBridge) queryCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.queries)
}

func (b *fakeBridge) dropAfterNextReply() {
	b.mu.Lock()
	b.closeNow = true
	b.mu.Unlock()
}

func (b *fakeBridge) keepAlive() {
	b.mu.Lock()
	b.closeNow = false
	b.mu.Unlock()
}

func (b *fakeBridge) hostPort(t *testing.T) (string, int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(b.ln.Addr().String())
	if err != nil {
		t.Fatalf("split addr: %v", err)
	}
	port, _ := strconv.Atoi(portStr)
	return host, port
}

func settingsFor(host string, port int) *fakeSettings {
	return &fakeSettings{s: &settings.Settings{
		Cnc: settings.Cnc{
			Machines: []settings.Machine{{ID: "m1", Name: "test", Host: host, Port: port}},
		},
	}}
}

// startLink wires a Link at the bridge and waits for it to connect.
func startLink(t *testing.T, b *fakeBridge) *Link {
	t.Helper()
	host, port := b.hostPort(t)
	l := NewLink(settingsFor(host, port), "m1", nil)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); l.Stop() })
	l.Start(ctx)
	waitFor(t, 2*time.Second, "link up", func() bool { return l.State().Up })
	return l
}

func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func query(t *testing.T, l *Link, q int) *QueryResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	res, err := l.Query(ctx, q, nil)
	if err != nil {
		t.Fatalf("Query(Q%d): %v", q, err)
	}
	return res
}

// The headline property: many queries, ONE TCP connection. This is the
// whole point of the Link — the previous design dialled per query.
func TestLink_ReusesOneConnection(t *testing.T) {
	b := newFakeBridge(t)
	l := startLink(t, b)

	const n = 12
	for i := 0; i < n; i++ {
		res := query(t, l, 104)
		if !res.OK {
			t.Fatalf("query %d not OK: %+v", i, res)
		}
		if res.Value != "MEM" {
			t.Fatalf("query %d value = %q, want MEM", i, res.Value)
		}
	}

	if got := b.queryCount(); got != n {
		t.Fatalf("bridge saw %d queries, want %d", got, n)
	}
	if got := b.acceptCount(); got != 1 {
		t.Fatalf("bridge accepted %d connections, want exactly 1 "+
			"(per-query dialling is the bug this replaces)", got)
	}
}

// Alive must reflect real round-trips, not merely that the socket
// opened. This is the display bug: `connected` used to mean "an
// operator touched the dashboard recently".
func TestLink_AliveTracksRoundTrips(t *testing.T) {
	b := newFakeBridge(t)
	l := startLink(t, b)

	if l.Alive() {
		t.Fatal("Alive() should be false before any successful round-trip, " +
			"even though the TCP dial succeeded")
	}
	if res := query(t, l, 104); !res.OK {
		t.Fatalf("query failed: %+v", res)
	}
	if !l.Alive() {
		t.Fatal("Alive() should be true right after a successful round-trip")
	}
}

// A dead bridge must fail callers fast rather than parking them until
// their context expires — the aggregator polls on 4s contexts and would
// otherwise pile up goroutines.
func TestLink_FailsFastWhenDown(t *testing.T) {
	// Bind then immediately close so the port is almost certainly dead.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)
	_ = ln.Close()

	l := NewLink(settingsFor(host, port), "m1", nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); l.Stop() }()
	l.Start(ctx)

	// Give the supervisor a moment to fail its first dial and enter the
	// reject loop.
	waitFor(t, 2*time.Second, "link marked down", func() bool {
		return l.State().LastErr != ""
	})

	qctx, qcancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer qcancel()
	t0 := time.Now()
	res, err := l.Query(qctx, 104, nil)
	elapsed := time.Since(t0)

	if err != nil {
		t.Fatalf("Query should return a failed result, not an error: %v", err)
	}
	if res.OK {
		t.Fatal("Query against a dead bridge reported OK")
	}
	if elapsed > time.Second {
		t.Fatalf("Query took %s to fail; should fail fast via the reject loop", elapsed)
	}
	if l.Alive() {
		t.Fatal("Alive() must be false while the bridge is unreachable")
	}
}

// The link must heal itself after the bridge drops the connection —
// that is the "never have to connect manually" requirement.
func TestLink_ReconnectsAfterDrop(t *testing.T) {
	b := newFakeBridge(t)
	l := startLink(t, b)

	if res := query(t, l, 104); !res.OK {
		t.Fatalf("first query failed: %+v", res)
	}

	// Force the bridge to hang up right after the next reply.
	b.dropAfterNextReply()
	_ = query(t, l, 104) // may succeed or fail; the drop lands after it
	b.keepAlive()

	// The supervisor should redial (linkRetryFloor is 1s, jittered).
	waitFor(t, 8*time.Second, "reconnect", func() bool {
		if !l.State().Up {
			return false
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		res, err := l.Query(ctx, 104, nil)
		return err == nil && res.OK
	})

	if b.acceptCount() < 2 {
		t.Fatalf("expected a redial (accepts >= 2), got %d", b.acceptCount())
	}
}

// Spacing protects the RS-232 side. Two back-to-back queries must be
// separated by at least minQuerySpacing.
func TestLink_EnforcesQuerySpacing(t *testing.T) {
	b := newFakeBridge(t)
	l := startLink(t, b)

	query(t, l, 104)
	t0 := time.Now()
	query(t, l, 104)
	if elapsed := time.Since(t0); elapsed < minQuerySpacing {
		t.Fatalf("second query ran %s after the first; minQuerySpacing is %s",
			elapsed, minQuerySpacing)
	}
}

// A job runs on the Link's existing socket instead of dialling its own,
// and Q-code queries submitted during the job are still serviced (that
// is what pump() is for).
func TestLink_RunJobUsesSameConnectionAndPumpsQueries(t *testing.T) {
	b := newFakeBridge(t)
	l := startLink(t, b)
	query(t, l, 104) // establish + prove baseline

	acceptsBefore := b.acceptCount()

	// Submit a query from another goroutine while the job body runs; the
	// body must pump it or this never resolves.
	got := make(chan *QueryResult, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		res, err := l.Query(ctx, 201, nil)
		if err == nil {
			got <- res
		} else {
			got <- &QueryResult{Error: err.Error()}
		}
	}()

	err := l.RunJob(context.Background(), func(_ Conn, _ *bufio.Reader, pump func(), _ flowContext) error {
		// Give the query time to reach the inbox, then pump until it is
		// serviced or we give up.
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			pump()
			select {
			case res := <-got:
				got <- res
				return nil
			default:
			}
			time.Sleep(10 * time.Millisecond)
		}
		return fmt.Errorf("query was never pumped")
	})
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}

	res := <-got
	if !res.OK || res.Value != "TOOL, 3" {
		t.Fatalf("pumped query result = %+v, want OK with TOOL, 3", res)
	}
	if b.acceptCount() != acceptsBefore {
		t.Fatalf("job opened a new connection (accepts %d → %d); it must reuse the link's socket",
			acceptsBefore, b.acceptCount())
	}
}

// A job must be refused outright when the bridge is unreachable — half
// -sending a program to a mill is the failure mode worth being loud about.
func TestLink_RunJobRefusedWhenDown(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)
	_ = ln.Close()

	l := NewLink(settingsFor(host, port), "m1", nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); l.Stop() }()
	l.Start(ctx)
	waitFor(t, 2*time.Second, "link down", func() bool { return l.State().LastErr != "" })

	jctx, jcancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer jcancel()
	err := l.RunJob(jctx, func(Conn, *bufio.Reader, func(), flowContext) error {
		t.Error("job body must not run against a dead link")
		return nil
	})
	if err == nil {
		t.Fatal("RunJob should fail when the link is down")
	}
}

// Regression guard for the persistent-reader hazard: exchangeOnConn
// allocated a fresh bufio.Reader per call, so anything it buffered past
// the current frame was silently dropped. On a long-lived socket that is
// data loss. exchangeOnReader must carry the buffer across exchanges.
func TestExchangeOnReader_PreservesBufferedBytesAcrossExchanges(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	// The server blurts BOTH replies in a single write, so the first
	// read is very likely to buffer the second frame too.
	go func() {
		r := bufio.NewReader(server)
		if _, err := r.ReadString('\n'); err != nil {
			return
		}
		_, _ = server.Write([]byte("\x02MEM\x17\r\n>\x02TOOL, 3\x17\r\n>"))
		// Answer the second query with nothing — if the buffered frame
		// was dropped, the second exchange has no source of bytes and
		// must time out.
		if _, err := r.ReadString('\n'); err != nil {
			return
		}
		select {}
	}()

	br := bufio.NewReader(client)

	raw1, err := exchangeOnReader(client, br, 104, nil, nil)
	if err != nil {
		t.Fatalf("first exchange: %v", err)
	}
	if v := stripEchoAndFraming(raw1); v != "MEM" {
		t.Fatalf("first value = %q, want MEM", v)
	}

	raw2, err := exchangeOnReader(client, br, 201, nil, nil)
	if err != nil {
		t.Fatalf("second exchange (buffered frame was dropped): %v", err)
	}
	if v := stripEchoAndFraming(raw2); v != "TOOL, 3" {
		t.Fatalf("second value = %q, want TOOL, 3", v)
	}
}

// Cancelling a job must not let RunJob return while the body is still
// writing. Streamer.Stop() reports "the socket is freed" off the back of
// this; returning early would mean G-code lines still going to the mill
// after the operator was told the job stopped.
func TestLink_RunJobWaitsForBodyAfterCancel(t *testing.T) {
	b := newFakeBridge(t)
	l := startLink(t, b)

	ctx, cancel := context.WithCancel(context.Background())
	bodyExited := make(chan struct{})
	started := make(chan struct{})

	go func() {
		time.Sleep(50 * time.Millisecond)
		close(started)
		cancel()
	}()

	err := l.RunJob(ctx, func(Conn, *bufio.Reader, func(), flowContext) error {
		<-started
		// Simulate a body that keeps working briefly after cancellation,
		// exactly as streamFile does between line boundaries.
		time.Sleep(200 * time.Millisecond)
		close(bodyExited)
		return nil
	})

	select {
	case <-bodyExited:
		// Correct: RunJob returned only after the body finished.
	default:
		t.Fatal("RunJob returned while the job body was still running — " +
			"Stop() would report the mill idle mid-stream")
	}
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}
}

// silentBridge accepts TCP and answers nothing — exactly what a
// Waveshare does when the mill behind it is powered off.
type silentBridge struct {
	ln      net.Listener
	mu      sync.Mutex
	accepts int
}

func newSilentBridge(t *testing.T) *silentBridge {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	b := &silentBridge{ln: ln}
	go func() {
		var held []net.Conn
		defer func() {
			for _, c := range held {
				_ = c.Close()
			}
		}()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			b.mu.Lock()
			b.accepts++
			b.mu.Unlock()
			held = append(held, conn) // hold it open, never reply
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return b
}

func (b *silentBridge) acceptCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.accepts
}

// A silent controller must NOT cause a reconnect storm. The socket is
// healthy; only the mill is off. Redialling on every timed-out query
// would reintroduce exactly the churn the Link exists to remove — all
// night, every night.
func TestLink_SilentControllerDoesNotChurnConnections(t *testing.T) {
	b := newSilentBridge(t)
	host, portStr, _ := net.SplitHostPort(b.ln.Addr().String())
	port, _ := strconv.Atoi(portStr)

	l := NewLink(settingsFor(host, port), "m1", nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); l.Stop() }()
	l.Start(ctx)
	waitFor(t, 2*time.Second, "link up", func() bool { return l.State().Up })

	for i := 0; i < 3; i++ {
		qctx, qcancel := context.WithTimeout(context.Background(), 6*time.Second)
		res, err := l.Query(qctx, 104, nil)
		qcancel()
		if err != nil {
			t.Fatalf("query %d returned transport error: %v", i, err)
		}
		if res.OK {
			t.Fatalf("query %d unexpectedly OK against a silent bridge", i)
		}
	}

	if got := b.acceptCount(); got != 1 {
		t.Fatalf("silent controller caused %d connections; want 1 "+
			"(timeouts must not drop a healthy socket)", got)
	}
	if l.Alive() {
		t.Fatal("Alive() must be false when the controller never answers, " +
			"even though the TCP connection is established")
	}
	if !l.State().Up {
		t.Fatal("link should still report Up — the socket is fine, the mill is not")
	}
}
