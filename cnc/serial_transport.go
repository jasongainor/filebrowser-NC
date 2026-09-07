package cnc

// Direct serial transport — a Raspberry Pi with a USB→RS-232 adapter
// owning the Haas' RS-232 header, replacing the Waveshare bridge. See
// docs/SERIAL_TRANSPORT.md for wiring + the Haas Setting mapping, and
// settings.Machine.EffectiveSerial for the default values baked in here.

import (
	"fmt"
	"strings"
	"time"

	"go.bug.st/serial"

	"github.com/filebrowser/filebrowser/v2/settings"
)

// serialTimeoutError is returned by serialConn.Read when
// go.bug.st/serial's read timeout elapses. go.bug.st/serial signals a
// timeout by returning (0, nil) from Port.Read (confirmed against
// v1.6.4's unixPort.Read: the select() timeout branch returns `0, nil`,
// while an actual disconnect returns a distinct PortError) — that shape
// doesn't carry a Timeout() bool the way net.Conn errors do, and every
// caller upstream (qcode.go's exchangeOnReader, dprnt.go's
// scavengeOnce) distinguishes "no data yet" from a real error by
// checking for a timeout. serialConn.Read re-shapes the (0, nil)
// signal into this error so that check keeps working unmodified for
// both transports.
type serialTimeoutError struct{}

func (serialTimeoutError) Error() string   { return "serial: read timeout" }
func (serialTimeoutError) Timeout() bool   { return true }
func (serialTimeoutError) Temporary() bool { return true }

var errSerialTimeout = serialTimeoutError{}

// serialTransport dials a tty using the Mode resolved from
// Machine.EffectiveSerial.
type serialTransport struct {
	device      string
	mode        *serial.Mode
	flowControl string // normalized: "xonxoff" | "rtscts" | "none"
}

// newSerialTransport validates Machine m's serial config and builds a
// serialTransport. Returns an error for any field that doesn't map to a
// value go.bug.st/serial understands — surfaced through
// Link.supervise's markDown/backoff path exactly like a bad host/port
// would be for TCP.
func newSerialTransport(m settings.Machine) (*serialTransport, error) {
	es := m.EffectiveSerial()

	mode := &serial.Mode{BaudRate: es.Baud, DataBits: es.DataBits}

	switch strings.ToLower(strings.TrimSpace(es.Parity)) {
	case "even":
		mode.Parity = serial.EvenParity
	case "odd":
		mode.Parity = serial.OddParity
	case "none", "n":
		mode.Parity = serial.NoParity
	case "mark":
		mode.Parity = serial.MarkParity
	case "space":
		mode.Parity = serial.SpaceParity
	default:
		return nil, fmt.Errorf("serial: machine %q: unknown parity %q (want even/odd/none/mark/space)", m.ID, es.Parity)
	}

	switch es.StopBits {
	case 1:
		mode.StopBits = serial.OneStopBit
	case 2:
		mode.StopBits = serial.TwoStopBits
	default:
		return nil, fmt.Errorf("serial: machine %q: unsupported stop bits %d (want 1 or 2)", m.ID, es.StopBits)
	}

	fc := strings.ToLower(strings.TrimSpace(es.FlowControl))
	switch fc {
	case "xonxoff", "rtscts", "none":
	default:
		return nil, fmt.Errorf("serial: machine %q: unknown flowControl %q (want xonxoff/rtscts/none)", m.ID, es.FlowControl)
	}

	return &serialTransport{device: es.Device, mode: mode, flowControl: fc}, nil
}

// Dial opens the tty. timeout is unused — go.bug.st/serial's Open is a
// local device open, not a network round-trip, so there is nothing to
// bound the way linkDialTimeout bounds a TCP dial.
func (t *serialTransport) Dial(_ time.Duration) (Conn, error) {
	port, err := serial.Open(t.device, t.mode)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", t.device, err)
	}
	if t.flowControl == "rtscts" {
		// Assert RTS ("I'm ready to receive") once at open. See
		// awaitClearToSend in flowcontrol.go for why this is a
		// software poll of the modem status bits rather than kernel
		// CRTSCTS — go.bug.st/serial's Mode has no field for it and
		// nativeOpen actively disables termios CRTSCTS.
		if err := port.SetRTS(true); err != nil {
			_ = port.Close()
			return nil, fmt.Errorf("serial: enable RTS on %s: %w", t.device, err)
		}
	}
	return &serialConn{port: port}, nil
}

func (t *serialTransport) Addr() string { return t.device }

func (t *serialTransport) FlowControl() string { return t.flowControl }

// serialConn adapts go.bug.st/serial's Port to the Conn interface.
type serialConn struct {
	port serial.Port
}

// Read re-shapes go.bug.st/serial's (0, nil) read-timeout signal into
// errSerialTimeout — see the comment on serialTimeoutError.
func (c *serialConn) Read(p []byte) (int, error) {
	n, err := c.port.Read(p)
	if err == nil && n == 0 && len(p) > 0 {
		return 0, errSerialTimeout
	}
	return n, err
}

func (c *serialConn) Write(p []byte) (int, error) { return c.port.Write(p) }

func (c *serialConn) Close() error { return c.port.Close() }

// SetReadDeadline converts an absolute deadline into the duration
// go.bug.st/serial's SetReadTimeout wants. A zero Time (Go's "no
// deadline" convention, matching net.Conn) maps to serial.NoTimeout
// (block forever), same as clearing a net.Conn deadline.
func (c *serialConn) SetReadDeadline(t time.Time) error {
	if t.IsZero() {
		return c.port.SetReadTimeout(serial.NoTimeout)
	}
	d := time.Until(t)
	if d <= 0 {
		// Already past the deadline — smallest positive timeout so the
		// next Read returns (almost) immediately via errSerialTimeout
		// rather than erroring on a negative duration.
		d = time.Millisecond
	}
	return c.port.SetReadTimeout(d)
}

// SetWriteDeadline is a deliberate no-op. go.bug.st/serial's Port has no
// write-timeout primitive, and unlike TCP a serial Write to a healthy
// port essentially never blocks indefinitely at the byte rates this
// project drip-feeds at (the failure mode that DOES stall writes —
// Setting 14 = XON/XOFF with the Haas asserting XOFF — is handled
// explicitly and visibly by flowGate in flowcontrol.go / streamFile's
// awaitFlowControlResume, not by a deadline here).
func (c *serialConn) SetWriteDeadline(time.Time) error { return nil }

func (c *serialConn) SetDeadline(t time.Time) error { return c.SetReadDeadline(t) }

// ClearToSend reports the controller's CTS modem-status bit. Implements
// ctsAware (flowcontrol.go) so streamFile can honor FlowControl ==
// "rtscts" via a type assertion — TCP conns don't implement this, so
// the assertion naturally no-ops for the bridge path.
func (c *serialConn) ClearToSend() (bool, error) {
	bits, err := c.port.GetModemStatusBits()
	if err != nil {
		return false, err
	}
	return bits.CTS, nil
}
