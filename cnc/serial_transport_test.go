package cnc

// Tests for the direct-serial transport (cnc/transport.go,
// cnc/serial_transport.go). Uses github.com/creack/pty to get a real
// tty pair: serial.Open performs genuine termios/ioctl calls, and a
// Linux pty slave accepts those the same way a USB-serial adapter's
// /dev/ttyUSB0 does — the one thing it can't do is modem-status lines
// (GetModemStatusBits fails with "inappropriate ioctl for device" on a
// pty), which is why RTS/CTS has no automated coverage here; see
// docs/SERIAL_TRANSPORT.md.
//
// No real Haas or Waveshare bridge is touched by anything in this file.

import (
	"bufio"
	"errors"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
	"go.bug.st/serial"

	"github.com/filebrowser/filebrowser/v2/settings"
)

// openPtySerialConn opens a pty pair and a real serial.Port on the
// slave side, wrapped in our serialConn. Returns the master end (what
// the test uses to play "Haas") and the client Conn (what
// link.go/streamer.go would hold). Both are closed via t.Cleanup.
func openPtySerialConn(t *testing.T, mode *serial.Mode) (master *os.File, conn *serialConn) {
	t.Helper()
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Fatalf("pty.Open: %v", err)
	}
	t.Cleanup(func() { _ = ptmx.Close() })
	t.Cleanup(func() { _ = tty.Close() })

	port, err := serial.Open(tty.Name(), mode)
	if err != nil {
		t.Fatalf("serial.Open(%s): %v", tty.Name(), err)
	}
	c := &serialConn{port: port}
	t.Cleanup(func() { _ = c.Close() })
	return ptmx, c
}

func defaultTestMode() *serial.Mode {
	return &serial.Mode{
		BaudRate: 9600,
		DataBits: 7,
		Parity:   serial.EvenParity,
		StopBits: serial.OneStopBit,
	}
}

// TestSerialConn_QCodeRoundTrip exercises the real serial transport —
// serial.Open on a pty slave, wrapped in serialConn — through the same
// exchangeOnReader path the Link uses for every Q-code query. This is
// the "over the serial transport" half of the required coverage;
// TestExchangeOnReader_StripsXonXoffWithGate (flowcontrol_test.go)
// covers the XON/XOFF stripping half.
func TestSerialConn_QCodeRoundTrip(t *testing.T) {
	master, conn := openPtySerialConn(t, defaultTestMode())

	go func() {
		r := bufio.NewReader(master)
		// Real wire format: "?Q104\r\n" in, "\x02MEM\x17\r\n>" out.
		line, err := r.ReadString('\n')
		if err != nil || !strings.HasPrefix(line, "?Q104") {
			return
		}
		_, _ = master.Write([]byte("\x02MEM\x17\r\n>"))
	}()

	br := bufio.NewReader(conn)
	raw, err := exchangeOnReader(conn, br, 104, nil, nil)
	if err != nil {
		t.Fatalf("exchangeOnReader over serial: %v", err)
	}
	if v := stripEchoAndFraming(raw); v != "MEM" {
		t.Fatalf("value = %q, want MEM (raw=%q)", v, raw)
	}
}

// TestSerialConn_ReadTimeoutMapsToTimeoutErr confirms serialConn.Read
// re-shapes go.bug.st/serial's (0, nil) timeout signal into an error
// that satisfies timeoutErr — the contract qcode.go and dprnt.go both
// depend on to tell "no data yet" apart from a real failure. Without
// this mapping the (0, nil) result would look like a valid empty read
// to callers written against net.Conn semantics.
func TestSerialConn_ReadTimeoutMapsToTimeoutErr(t *testing.T) {
	_, conn := openPtySerialConn(t, defaultTestMode())

	if err := conn.SetReadDeadline(time.Now().Add(30 * time.Millisecond)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	buf := make([]byte, 16)
	n, err := conn.Read(buf)
	if n != 0 {
		t.Fatalf("expected 0 bytes on a silent line, got %d", n)
	}
	if err == nil {
		t.Fatal("expected a timeout error, got nil")
	}
	var te timeoutErr
	if !errors.As(err, &te) || !te.Timeout() {
		t.Fatalf("error %v does not satisfy timeoutErr", err)
	}
}

// TestNewSerialTransport_AppliesHaasDefaults checks EffectiveSerial's
// zero-value defaults (9600 7E1 xonxoff) make it all the way into the
// serial.Mode newSerialTransport builds, and that FlowControl()
// reflects the normalized value.
func TestNewSerialTransport_AppliesHaasDefaults(t *testing.T) {
	m := settings.Machine{ID: "m1", Serial: settings.MachineSerial{Device: "/dev/ttyUSB0"}}
	tr, err := newSerialTransport(m)
	if err != nil {
		t.Fatalf("newSerialTransport: %v", err)
	}
	if tr.mode.BaudRate != 9600 {
		t.Errorf("BaudRate = %d, want 9600", tr.mode.BaudRate)
	}
	if tr.mode.DataBits != 7 {
		t.Errorf("DataBits = %d, want 7", tr.mode.DataBits)
	}
	if tr.mode.Parity != serial.EvenParity {
		t.Errorf("Parity = %v, want EvenParity", tr.mode.Parity)
	}
	if tr.mode.StopBits != serial.OneStopBit {
		t.Errorf("StopBits = %v, want OneStopBit", tr.mode.StopBits)
	}
	if tr.FlowControl() != "xonxoff" {
		t.Errorf("FlowControl() = %q, want xonxoff", tr.FlowControl())
	}
	if tr.Addr() != "/dev/ttyUSB0" {
		t.Errorf("Addr() = %q, want /dev/ttyUSB0", tr.Addr())
	}
}

// TestNewSerialTransport_RejectsBadConfig checks each field's error
// path — an operator typo here should fail loudly (via Link's
// markDown/backoff) rather than silently falling back to a default.
func TestNewSerialTransport_RejectsBadConfig(t *testing.T) {
	base := settings.Machine{ID: "m1", Serial: settings.MachineSerial{Device: "/dev/ttyUSB0"}}

	cases := []struct {
		name string
		mut  func(s *settings.MachineSerial)
	}{
		{"bad parity", func(s *settings.MachineSerial) { s.Parity = "reversed" }},
		{"bad stop bits", func(s *settings.MachineSerial) { s.StopBits = 3 }},
		{"bad flow control", func(s *settings.MachineSerial) { s.FlowControl = "carrier-pigeon" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := base
			c.mut(&m.Serial)
			if _, err := newSerialTransport(m); err == nil {
				t.Fatal("expected an error, got nil")
			}
		})
	}
}

// TestBuildTransport_PicksSerialOrTCP confirms the dispatch rule: a
// Serial.Device set means serial, empty means the pre-existing TCP
// path — and that TCP's shape (addr, no flow control) is unchanged.
func TestBuildTransport_PicksSerialOrTCP(t *testing.T) {
	tcpMachine := settings.Machine{Host: "10.0.0.5"}
	tr, err := buildTransport(tcpMachine, 4196)
	if err != nil {
		t.Fatalf("buildTransport(tcp): %v", err)
	}
	if _, ok := tr.(tcpTransport); !ok {
		t.Fatalf("expected tcpTransport, got %T", tr)
	}
	if tr.Addr() != "10.0.0.5:4196" {
		t.Errorf("Addr() = %q, want 10.0.0.5:4196", tr.Addr())
	}
	if tr.FlowControl() != "" {
		t.Errorf("FlowControl() = %q, want \"\" for TCP", tr.FlowControl())
	}

	serialMachine := settings.Machine{Serial: settings.MachineSerial{Device: "/dev/ttyUSB0"}}
	tr2, err := buildTransport(serialMachine, 0)
	if err != nil {
		t.Fatalf("buildTransport(serial): %v", err)
	}
	if _, ok := tr2.(*serialTransport); !ok {
		t.Fatalf("expected *serialTransport, got %T", tr2)
	}
}

// Compile-time-ish check that net.Conn keeps satisfying Conn — a
// regression here would mean the TCP path silently broke.
var _ Conn = (net.Conn)(nil)
