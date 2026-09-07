package cnc

// Software XON/XOFF flow control for the DNC drip-feed path over a
// direct serial connection (cnc/serial_transport.go). The Waveshare
// bridge could never carry this end-to-end — TCP has no side channel
// for an in-band control byte to pre-empt a write already queued in the
// kernel's TCP send buffer, so a real tty is what makes Haas Setting 14
// (Synchronization/Handshake) actually work. See
// docs/PROGRAM_DELIVERY_AND_LIBRARY_SYNC_TODO.md section B and
// docs/SERIAL_TRANSPORT.md.
//
// The Haas asserts XOFF (0x13) in-band on the RS-232 line when its
// receive buffer nears full, and XON (0x11) once it has room again.
// flowGate tracks that state; every read of a serial connection — the
// DPRNT scavenger, exchangeOnReader's Q-code framing, and the
// dedicated poll below — routes bytes through gate.filter so control
// bytes are stripped before they can land in captured text, and
// streamFile checks gate.isPaused before writing the next G-code line.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

const (
	xonByte  = 0x11
	xoffByte = 0x13
)

// ErrFlowControlStalled is returned when the controller asserts XOFF
// (or, for rtscts, drops CTS) and never resumes within
// flowControlResumeTimeout. That is not a normal pause — it means the
// cable, the control, or the flow-control settings themselves are
// wrong — so streamFile aborts the job instead of hanging forever.
var ErrFlowControlStalled = errors.New("serial flow control: controller did not resume (XON / CTS) before timeout")

// flowControlResumeTimeout bounds how long streamFile will wait for a
// resume signal after seeing XOFF (or CTS low) before giving up.
// Generous: a Haas can legitimately hold its receive buffer full for
// tens of seconds while it machines a dense block of moves before the
// next drip-feed line is consumed.
//
// Deliberately a var, not a const: flowcontrol_test.go shrinks it to
// exercise the ErrFlowControlStalled path in milliseconds instead of a
// full minute.
var flowControlResumeTimeout = 60 * time.Second

// flowPollInterval is how often awaitFlowControlResume re-checks state
// once paused. Short enough not to add noticeable latency once the
// controller resumes, long enough not to busy-spin the CPU or hammer
// the serial ioctl.
const flowPollInterval = 20 * time.Millisecond

// flowReadDeadline bounds each individual poll read used to catch a
// resume signal or to scavenge XON/XOFF bytes between line writes.
// Matches dprntScavengeDeadline's reasoning: short enough to not stall
// the write loop, long enough to usually catch a byte that's already
// arrived.
const flowReadDeadline = 3 * time.Millisecond

// flowGate tracks XON/XOFF pause state for one serial connection's
// lifetime. Built fresh per successful dial (Link.supervise) — a
// redial starts unpaused, matching a freshly opened tty.
type flowGate struct {
	mu     sync.Mutex
	paused bool
}

func newFlowGate() *flowGate { return &flowGate{} }

func (g *flowGate) isPaused() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.paused
}

func (g *flowGate) setPaused(p bool) {
	g.mu.Lock()
	g.paused = p
	g.mu.Unlock()
}

// filter scans buf for XON/XOFF control bytes, updates paused
// accordingly, and returns buf with those bytes stripped so callers
// never see them as DPRNT/Q-code payload. May reuse buf's backing array
// (the result is always the same length or shorter).
func (g *flowGate) filter(buf []byte) []byte {
	if !bytes.ContainsAny(buf, "\x11\x13") {
		return buf
	}
	out := buf[:0]
	for _, b := range buf {
		switch b {
		case xoffByte:
			g.setPaused(true)
		case xonByte:
			g.setPaused(false)
		default:
			out = append(out, b)
		}
	}
	return out
}

// flowContext carries one connection's flow-control configuration and
// (for xonxoff) live pause state into a streaming job body. Mode "" or
// "none" means no flow-control handling applies — TCP, or serial
// explicitly configured with FlowControl: "none". Gate is non-nil only
// when Mode == "xonxoff".
type flowContext struct {
	Mode string
	Gate *flowGate
}

// ctsAware is implemented by serialConn so streamFile can honor
// FlowControl == "rtscts" via a plain type assertion, without this file
// importing the serial package. TCP connections don't implement it, so
// the assertion no-ops for the bridge path.
//
// NOTE on what this actually is: go.bug.st/serial's Mode struct has no
// field for hardware flow control, and its nativeOpen unconditionally
// disables termios CRTSCTS ("Explicitly disable RTS/CTS flow control").
// There is no public API in that library to get real interrupt-driven
// hardware handshake. What ClearToSend gives us instead is a software
// poll of the CTS modem-status bit — on a null-modem cable the far
// end's RTS output is wired to our CTS input, so this does reflect the
// controller's real "ready to receive" signal, just checked on our
// schedule (flowPollInterval) rather than reacted to instantly. That is
// adequate for line-at-a-time DNC drip-feed. It is NOT tested against
// real hardware (see docs/SERIAL_TRANSPORT.md): a pty pair, which is
// all the test suite has, has no modem-status lines
// (GetModemStatusBits fails with "inappropriate ioctl for device" on
// one), so this path has no automated coverage.
type ctsAware interface {
	ClearToSend() (bool, error)
}

// timeoutErr is the structural interface both net.Conn's errors and
// serialTimeoutError satisfy. Using this instead of net.Error directly
// lets qcode.go / dprnt.go / flowcontrol.go treat a serial read timeout
// exactly like a TCP one without importing net.
type timeoutErr interface {
	Timeout() bool
}

// pollFlowControl performs one short, bounded read to catch any
// XON/XOFF bytes the controller has sent since the last line, updating
// gate's paused state. Bytes read are discarded (not surfaced as
// DPRNT). Used when DPRNTCapture is off — when it's on,
// dprntBuffer.scavengeOnce already applies this same filtering as a
// side effect of pulling DPRNT text, so streamFile does not double up.
func pollFlowControl(conn Conn, r io.Reader, gate *flowGate) {
	if err := conn.SetReadDeadline(time.Now().Add(flowReadDeadline)); err != nil {
		return
	}
	defer func() { _ = conn.SetReadDeadline(time.Time{}) }()

	tmp := make([]byte, 256)
	n, _ := r.Read(tmp)
	if n > 0 {
		gate.filter(tmp[:n])
	}
}

// awaitFlowControlResume blocks while gate reports paused (XOFF seen,
// XON not yet), polling reads on conn/r to catch the XON. Returns nil
// immediately if not paused, nil early on ctx cancellation (an operator
// Stop wins over a flow-control wait — streamFile's caller checks ctx
// again on the next loop iteration and exits cleanly), and
// ErrFlowControlStalled if flowControlResumeTimeout elapses first.
func awaitFlowControlResume(ctx context.Context, conn Conn, r io.Reader, gate *flowGate, logf func(level, format string, args ...any)) error {
	if !gate.isPaused() {
		return nil
	}
	if logf != nil {
		logf("warn", "serial flow control: paused for XOFF from controller")
	}
	deadline := time.Now().Add(flowControlResumeTimeout)
	for gate.isPaused() {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		if time.Now().After(deadline) {
			return ErrFlowControlStalled
		}
		if err := conn.SetReadDeadline(time.Now().Add(flowPollInterval)); err != nil {
			return fmt.Errorf("serial flow control: %w", err)
		}
		tmp := make([]byte, 64)
		n, err := r.Read(tmp)
		_ = conn.SetReadDeadline(time.Time{})
		if n > 0 {
			gate.filter(tmp[:n])
		}
		if err != nil {
			if te, ok := err.(timeoutErr); ok && te.Timeout() {
				continue
			}
			if errors.Is(err, io.EOF) {
				continue
			}
			return fmt.Errorf("serial flow control: read while paused: %w", err)
		}
	}
	if logf != nil {
		logf("info", "serial flow control: resumed after XON")
	}
	return nil
}

// awaitClearToSend blocks (bounded by flowControlResumeTimeout) until
// conn reports CTS asserted, for machines configured with Setting 14 =
// RTS/CTS. See ctsAware's doc comment for what this actually is (a
// software poll, not kernel CRTSCTS) and its testing gap. A conn that
// doesn't implement ctsAware (TCP) is a no-op.
func awaitClearToSend(ctx context.Context, conn Conn, logf func(level, format string, args ...any)) error {
	cts, ok := conn.(ctsAware)
	if !ok {
		return nil
	}
	ready, err := cts.ClearToSend()
	if err != nil {
		return fmt.Errorf("serial flow control: read CTS: %w", err)
	}
	if ready {
		return nil
	}
	if logf != nil {
		logf("warn", "serial flow control: paused, CTS low")
	}
	deadline := time.Now().Add(flowControlResumeTimeout)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(flowPollInterval):
		}
		ready, err := cts.ClearToSend()
		if err != nil {
			return fmt.Errorf("serial flow control: read CTS: %w", err)
		}
		if ready {
			if logf != nil {
				logf("info", "serial flow control: resumed, CTS high")
			}
			return nil
		}
		if time.Now().After(deadline) {
			return ErrFlowControlStalled
		}
	}
}
