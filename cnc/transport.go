package cnc

// Transport abstraction — the Link used to dial net.Dial("tcp", ...)
// directly. This file introduces a Conn/Transport seam so the Link can
// hold either a TCP socket to the Waveshare RS-232↔TCP bridge (the only
// option until now) or a direct serial connection to the Haas' own
// RS-232 header (serial_transport.go), decided per-Machine by whether
// settings.Machine.Serial.Device is set.
//
// Why this exists: the Waveshare bridge cannot carry hardware or
// software flow control end-to-end — see
// docs/PROGRAM_DELIVERY_AND_LIBRARY_SYNC_TODO.md section B. A Raspberry
// Pi with a USB→RS-232 adapter wired straight to the Haas gets Setting
// 14 (XON/XOFF) for real, which is what unattended DNC drip-feed
// actually needs.

import (
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/filebrowser/filebrowser/v2/settings"
)

// Conn is the minimal duplex byte-stream contract the Link, streamer,
// and qcode/dprnt read paths need. net.Conn already satisfies this
// (see the compile-time assertion below); serialConn adapts
// go.bug.st/serial's Port to the same shape so none of link.go,
// streamer.go, qcode.go, or dprnt.go need to know which transport they
// are holding.
type Conn interface {
	Read(p []byte) (int, error)
	Write(p []byte) (int, error)
	Close() error
	SetDeadline(t time.Time) error
	SetReadDeadline(t time.Time) error
	SetWriteDeadline(t time.Time) error
}

// Compile-time check: every net.Conn (the TCP path) already implements
// Conn without any adapter.
var _ Conn = net.Conn(nil)

// Transport dials one connection to a machine. TCP (this file) and
// serial (serial_transport.go) both implement it; Link.supervise calls
// Dial in its reconnect loop instead of net.DialTimeout directly.
type Transport interface {
	// Dial opens the connection. timeout bounds the attempt; for the
	// serial transport it is effectively unused since opening a tty is
	// not a network round-trip.
	Dial(timeout time.Duration) (Conn, error)
	// Addr is a human string for logs/LinkState — "host:port" for TCP,
	// the tty device path for serial.
	Addr() string
	// FlowControl reports the configured software flow-control mode:
	// "" for TCP (not applicable), or the normalized
	// serial.Machine.Serial.FlowControl value ("xonxoff" | "rtscts" |
	// "none") for serial. The Link uses this to decide whether to
	// stand up a flowGate for the connection's lifetime.
	FlowControl() string
}

// tcpTransport is the original (and still default) behavior: dial the
// Waveshare RS-232↔TCP bridge. Byte-identical to the pre-Transport code
// in link.go.
type tcpTransport struct {
	addr string
}

func (t tcpTransport) Dial(timeout time.Duration) (Conn, error) {
	return net.DialTimeout("tcp", t.addr, timeout)
}

func (t tcpTransport) Addr() string { return t.addr }

func (t tcpTransport) FlowControl() string { return "" }

// buildTransport picks TCP or serial for Machine m based on whether
// Serial.Device is set. port is the already-defaulted TCP port (see
// resolveMachine); it is ignored for the serial path.
func buildTransport(m settings.Machine, port int) (Transport, error) {
	if strings.TrimSpace(m.Serial.Device) != "" {
		return newSerialTransport(m)
	}
	addr := net.JoinHostPort(m.Host, strconv.Itoa(port))
	return tcpTransport{addr: addr}, nil
}
