package cnc

// Tests for software XON/XOFF flow control (cnc/flowcontrol.go). The
// pure-logic pieces (flowGate.filter) are tested with plain byte
// slices; the wait/resume pieces use net.Pipe, which — like a real
// serial line here — is just something implementing Conn that the test
// can write control bytes into on a schedule. The full
// TestStreamFile_PausesForXOFFThenSendsInOrder test additionally proves
// the pause/resume integrates correctly with streamFile's actual line
// writes, including byte-order preservation across the pause.
//
// No real Haas or Waveshare bridge is touched by anything in this file.

import (
	"bufio"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestFlowGate_FilterStripsControlBytesAndTracksState(t *testing.T) {
	g := newFlowGate()

	out := g.filter([]byte("AB\x13CD\x11EF"))
	if string(out) != "ABCDEF" {
		t.Fatalf("filter output = %q, want ABCDEF", out)
	}
	if g.isPaused() {
		t.Fatal("expected unpaused: XOFF was followed by XON in the same chunk")
	}

	// XOFF with no matching XON in this chunk — must stay paused across
	// calls, exactly like a real Haas holding its buffer full across
	// several scavenge reads.
	out2 := g.filter([]byte("GH\x13IJ"))
	if string(out2) != "GHIJ" {
		t.Fatalf("filter output = %q, want GHIJ", out2)
	}
	if !g.isPaused() {
		t.Fatal("expected paused after a trailing XOFF")
	}

	out3 := g.filter([]byte("\x11KL"))
	if string(out3) != "KL" {
		t.Fatalf("filter output = %q, want KL", out3)
	}
	if g.isPaused() {
		t.Fatal("expected unpaused after XON arrives in a later chunk")
	}
}

func TestFlowGate_FilterNoOpWhenNoControlBytes(t *testing.T) {
	g := newFlowGate()
	in := []byte("plain text, no control bytes")
	out := g.filter(in)
	if string(out) != string(in) {
		t.Fatalf("filter output = %q, want unchanged %q", out, in)
	}
}

// TestExchangeOnReader_StripsXonXoffWithGate proves the Q-code read
// path (link.go's exchangeOnReader) never lets XON/XOFF bytes into a
// captured frame, and updates gate state as it consumes them — even
// when they arrive spliced INTO the middle of a response, which is
// exactly where a real Haas would insert them (flow control is
// asserted whenever its buffer nears full, without regard for what
// byte stream it interrupts).
func TestExchangeOnReader_StripsXonXoffWithGate(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	go func() {
		r := bufio.NewReader(server)
		if _, err := r.ReadString('\n'); err != nil {
			return
		}
		// XOFF then immediately XON before the framed reply — proves
		// both are consumed and stripped within a single exchange.
		_, _ = server.Write([]byte{xoffByte, xonByte})
		_, _ = server.Write([]byte("\x02MEM\x17\r\n>"))
	}()

	br := bufio.NewReader(client)
	gate := newFlowGate()
	raw, err := exchangeOnReader(client, br, 104, nil, gate)
	if err != nil {
		t.Fatalf("exchangeOnReader: %v", err)
	}
	if strings.ContainsAny(raw, "\x11\x13") {
		t.Fatalf("raw frame still contains XON/XOFF bytes: %q", raw)
	}
	if v := stripEchoAndFraming(raw); v != "MEM" {
		t.Fatalf("value = %q, want MEM (raw=%q)", v, raw)
	}
	if gate.isPaused() {
		t.Fatal("expected unpaused: the XOFF was immediately followed by an XON")
	}
}

// TestScavengeOnce_StripsXonXoffFromDPRNTText proves the DPRNT read
// path (dprnt.go's scavengeOnce) strips XON/XOFF the same way, so a
// captured DPRNT[…] line never contains a flow-control byte the
// controller happened to interleave with its output.
func TestScavengeOnce_StripsXonXoffFromDPRNTText(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	go func() {
		_, _ = server.Write([]byte{xoffByte})
		_, _ = server.Write([]byte("X1.234"))
		_, _ = server.Write([]byte{xonByte})
		_, _ = server.Write([]byte("Y5.678\n"))
	}()

	d := &dprntBuffer{}
	gate := newFlowGate()
	var emitted []string
	emit := func(s string) { emitted = append(emitted, s) }

	deadline := time.Now().Add(2 * time.Second)
	for len(emitted) == 0 && time.Now().Before(deadline) {
		if _, err := d.scavengeOnce(client, bufio.NewReader(client), emit, nil, gate); err != nil {
			t.Fatalf("scavengeOnce: %v", err)
		}
	}
	if !reflect.DeepEqual(emitted, []string{"X1.234Y5.678"}) {
		t.Fatalf("emitted = %v, want [X1.234Y5.678] (XON/XOFF must not appear in captured text)", emitted)
	}
	if gate.isPaused() {
		t.Fatal("expected unpaused after the trailing XON")
	}
}

// TestAwaitFlowControlResume_WaitsForXONThenResumes drives
// awaitFlowControlResume directly: gate starts paused (as if a prior
// scavenge/poll already saw XOFF), the peer sends XON after a known
// delay, and the wait must not return before that delay elapses.
func TestAwaitFlowControlResume_WaitsForXONThenResumes(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	gate := newFlowGate()
	gate.setPaused(true)

	const resumeDelay = 120 * time.Millisecond
	go func() {
		time.Sleep(resumeDelay)
		_, _ = server.Write([]byte{xonByte})
	}()

	br := bufio.NewReader(client)
	start := time.Now()
	if err := awaitFlowControlResume(context.Background(), client, br, gate, nil); err != nil {
		t.Fatalf("awaitFlowControlResume: %v", err)
	}
	elapsed := time.Since(start)

	if gate.isPaused() {
		t.Fatal("expected gate unpaused after XON")
	}
	if elapsed < resumeDelay-15*time.Millisecond {
		t.Fatalf("returned after %v, want >= ~%v (resumed before XON arrived)", elapsed, resumeDelay)
	}
}

// TestAwaitFlowControlResume_ContextCancelWinsOverPause confirms an
// operator Stop (ctx cancellation) is not blocked by an outstanding
// XOFF — streamFile relies on this to exit promptly even mid-pause.
func TestAwaitFlowControlResume_ContextCancelWinsOverPause(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	gate := newFlowGate()
	gate.setPaused(true)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	br := bufio.NewReader(client)
	start := time.Now()
	err := awaitFlowControlResume(ctx, client, br, gate, nil)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("expected nil (clean cancel), got %v", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("took %v — did not react to context cancellation promptly", elapsed)
	}
}

// TestAwaitFlowControlResume_StallTimesOut confirms a Haas that never
// sends XON produces ErrFlowControlStalled rather than hanging the job
// forever. Shrinks the package-level timeout var so this runs in
// milliseconds.
func TestAwaitFlowControlResume_StallTimesOut(t *testing.T) {
	orig := flowControlResumeTimeout
	flowControlResumeTimeout = 40 * time.Millisecond
	t.Cleanup(func() { flowControlResumeTimeout = orig })

	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	gate := newFlowGate()
	gate.setPaused(true)
	// Deliberately never send XON.

	br := bufio.NewReader(client)
	err := awaitFlowControlResume(context.Background(), client, br, gate, nil)
	if !errors.Is(err, ErrFlowControlStalled) {
		t.Fatalf("err = %v, want ErrFlowControlStalled", err)
	}
}

// TestStreamFile_PausesForXOFFThenSendsInOrder is the end-to-end DNC
// drip-feed check: streamFile (streamer.go) writes a 3-line program to
// a paused connection, must not write anything until XON arrives, and
// must then deliver every line to the peer in the original order with
// content intact.
func TestStreamFile_PausesForXOFFThenSendsInOrder(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "part.nc")
	if err := os.WriteFile(path, []byte("N1 G00 X0\nN2 G00 X1\nN3 G00 X2\n"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	s := newTestStreamer()
	j := &job{id: "job1", absPath: path, lineTotal: 3}
	br := bufio.NewReader(client)
	gate := newFlowGate()
	gate.setPaused(true) // controller is already XOFF'd before the job starts
	fc := flowContext{Mode: "xonxoff", Gate: gate}

	const resumeDelay = 100 * time.Millisecond
	go func() {
		time.Sleep(resumeDelay)
		_, _ = server.Write([]byte{xonByte})
	}()

	errCh := make(chan error, 1)
	start := time.Now()
	go func() {
		errCh <- s.streamFile(context.Background(), j, client, br, func() {}, fc)
	}()

	sr := bufio.NewReader(server)
	var got []string
	for i := 0; i < 3; i++ {
		line, err := sr.ReadString('\n')
		if err != nil {
			t.Fatalf("server read line %d: %v", i+1, err)
		}
		got = append(got, strings.TrimRight(line, "\r\n"))
	}
	elapsed := time.Since(start)

	if err := <-errCh; err != nil {
		t.Fatalf("streamFile: %v", err)
	}

	want := []string{"N1 G00 X0", "N2 G00 X1", "N3 G00 X2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v (byte order not preserved across the XOFF pause)", got, want)
	}
	if elapsed < resumeDelay-15*time.Millisecond {
		t.Fatalf("first line was written after %v, before the %v XON delay — pause was not honored", elapsed, resumeDelay)
	}
}
