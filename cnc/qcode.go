package cnc

// Q-code wire protocol — Go port of haas-dashboard's haas_bridge.py.
//
// Wire format (verified against a TM-2P over a Waveshare RS-232↔TCP):
//   send: "?Q<code>[ <var>]\r\n"
//   recv: "<echo>\r\r\n\x02<payload>\x17\r\n>\n"
// The payload is wrapped in STX (0x02) … ETB (0x17). The trailing ">?" /
// ">\n" is the *next* idle prompt — unreliable as an end marker. ETB is
// the truth; idle-after-data is the fallback for controls that don't
// frame.
//
// Pre-conditions on the Haas:
//   Setting 143 (Machine Data Collect) — ON. Without it Q-codes are no-ops.
//   Setting 187 (Echo) — either way is fine; the parser tolerates both.

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	stxByte = 0x02
	etbByte = 0x17

	// queryTimeout bounds a single Q-code round-trip (dial + write + read + close).
	queryTimeout = 3 * time.Second
	// idleAfterData backs off the read once we've already seen bytes — covers
	// the small handful of Haas controls that emit data without a closing ETB.
	idleAfterData = 1 * time.Second
)

// errNoResponse means the socket was fine but the controller said
// nothing within queryTimeout — typically the mill is powered off (the
// Waveshare stays up and keeps answering TCP), or Setting 143 is off.
//
// The Link treats this as NON-fatal to the connection. Dropping and
// redialling on every silent query would put us back in a reconnect
// storm all night, every night, which is the churn this design exists
// to eliminate. Genuine socket faults (write errors, EOF) still force a
// redial.
var errNoResponse = errors.New("no response")

// QueryResult mirrors the dashboard's contract so /api/cnc/qcode is a
// drop-in replacement for haas-dashboard's POST /api/query.
type QueryResult struct {
	Q          int     `json:"q"`
	Var        *int    `json:"var,omitempty"`
	Raw        string  `json:"raw"`
	Value      string  `json:"value"`
	Parsed     any     `json:"parsed,omitempty"`
	OK         bool    `json:"ok"`
	Error      string  `json:"error,omitempty"`
	DurationMs float64 `json:"duration_ms"`
}

// payloadFor builds the bytes we put on the wire. macroVar is optional;
// pass nil for plain queries like Q104 (mode) or Q500 (program/parts).
func payloadFor(qCode int, macroVar *int) []byte {
	var b strings.Builder
	b.WriteString("?Q")
	b.WriteString(strconv.Itoa(qCode))
	if macroVar != nil {
		b.WriteByte(' ')
		b.WriteString(strconv.Itoa(*macroVar))
	}
	b.WriteString("\r\n")
	return []byte(b.String())
}

// exchangeOnConn writes one query and reads one framed response on an
// already-open connection, using a reader private to this call.
//
// Only safe when the connection is about to be closed: any bytes the
// reader buffers past the current frame are discarded with it. On a
// long-lived connection use exchangeOnReader and hand it the reader
// that owns the socket for its whole lifetime.
// Kept although nothing calls it today: dprnt.go and link_test.go both cite it
// by name to explain the persistent-reader hazard, so deleting it would orphan
// the explanation of why exchangeOnReader exists.
//
//nolint:unused // documented counterpart to exchangeOnReader; see the comments above and in dprnt.go
func exchangeOnConn(conn Conn, qCode int, macroVar *int) (string, error) {
	return exchangeOnReader(conn, bufio.NewReader(conn), qCode, macroVar, nil)
}

// exchangeOnReader writes one query and reads one framed response,
// reading through the caller-supplied buffered reader. br MUST be the
// only reader on conn — see cnc/link.go, where a single reader is
// created per connection and reused for the socket's lifetime so no
// buffered bytes are ever stranded between exchanges.
//
// conn is used for deadline control only; all reads go through br.
//
// gate is non-nil only for a serial connection configured with
// FlowControl "xonxoff" (see cnc/flowcontrol.go). When set, XON (0x11)
// / XOFF (0x13) bytes are consumed to update gate's pause state and
// never appear in the returned frame — a Q-code response must never
// have a stray control byte land inside its STX…ETB payload. TCP
// callers pass nil and get byte-identical behavior to before this
// parameter existed.
func exchangeOnReader(conn Conn, br *bufio.Reader, qCode int, macroVar *int, gate *flowGate) (string, error) {
	deadline := time.Now().Add(queryTimeout)
	if err := conn.SetDeadline(deadline); err != nil {
		return "", err
	}
	defer func() {
		_ = conn.SetDeadline(time.Time{}) // clear so the streaming loop isn't capped
	}()

	if _, err := conn.Write(payloadFor(qCode, macroVar)); err != nil {
		return "", fmt.Errorf("write: %w", err)
	}

	// Read byte-at-a-time, stopping ON the ETB that closes our frame.
	//
	// This must not over-read. Pulling 512-byte chunks would routinely
	// swallow the START of the next response — on a throwaway socket
	// that was harmless (the connection died next), but this reader
	// outlives the exchange, so anything consumed past the frame is
	// data the NEXT query needed. Stopping exactly at ETB leaves the
	// remainder in br for whoever reads next. Bytes are cheap here:
	// br is buffered, so this is a memory copy, not a syscall per byte.
	var buf bytes.Buffer
	for {
		// Shorten the deadline only when we're actually about to block
		// on the socket (nothing left buffered) and already hold data —
		// covers controls that emit a payload with no closing ETB
		// without paying a setsockopt per byte.
		if buf.Len() > 0 && br.Buffered() == 0 {
			next := time.Now().Add(idleAfterData)
			if next.Before(deadline) {
				_ = conn.SetReadDeadline(next)
			}
		}
		c, err := br.ReadByte()
		if err == nil {
			if gate != nil {
				switch c {
				case xoffByte:
					gate.setPaused(true)
					continue
				case xonByte:
					gate.setPaused(false)
					continue
				}
			}
			buf.WriteByte(c)
			if c == etbByte {
				return buf.String(), nil
			}
			continue
		}
		if errors.Is(err, io.EOF) {
			if buf.Len() > 0 {
				return buf.String(), nil
			}
			return "", fmt.Errorf("read: %w", err)
		}
		// Timeout? If we have buffered bytes treat it as idle-done,
		// otherwise propagate. timeoutErr (cnc/flowcontrol.go) is a
		// structural stand-in for net.Error so this same check covers
		// both the TCP and serial transports without importing net.
		var te timeoutErr
		if errors.As(err, &te) && te.Timeout() {
			if buf.Len() > 0 {
				return buf.String(), nil
			}
			return "", fmt.Errorf("%w within %s", errNoResponse, queryTimeout)
		}
		return "", err
	}
}

// frameRe matches `\x02 … \x17` — the canonical Haas STX-framed payload.
var frameRe = regexp.MustCompile("\x02([^\x17]*)\x17")

// stripEchoAndFraming pulls the meaningful payload out of a raw response.
// Requires STX/ETB framing — historically we tolerated unframed line-mode
// responses for non-Haas controllers, but in practice the only way that
// path triggered was when the read buffer picked up G-code line bytes
// during a stream and we'd return them as if they were a Q response.
// Returning "" on no-frame lets validateResponseShape reject cleanly.
func stripEchoAndFraming(raw string) string {
	if raw == "" {
		return ""
	}
	if m := frameRe.FindStringSubmatch(raw); m != nil {
		return strings.TrimSpace(m[1])
	}
	return ""
}

// validateResponseShape returns nil if `value` plausibly answers the
// query we sent, or an error describing the mismatch. Catches bridge
// cross-talk: the Waveshare buffers RS-232 responses across TCP
// connection boundaries, so a fresh dial can return data left over
// from someone else's last query (or our own previous one, before
// minQuerySpacing kicked in). Without this check those stale frames
// would parse and display as truth.
//
// Rules are intentionally narrow — match the obvious tag word for
// each Q-code per haas_bridge.py + Haas docs. Macro queries are
// stricter: the response must carry the exact macro var number we
// asked for.
func validateResponseShape(qCode int, macroVar *int, value string) error {
	if value == "" {
		return fmt.Errorf("empty response")
	}
	upper := strings.ToUpper(value)
	switch qCode {
	case 104: // Mode — single token like MEM/MDI/JOG
		if strings.Contains(upper, "PROGRAM") ||
			strings.Contains(upper, "MACRO") ||
			strings.Contains(upper, "PARTS") ||
			strings.Contains(upper, "LAST CYCLE") ||
			strings.Contains(upper, "TOOL") {
			return fmt.Errorf("Q104 (mode) got cross-talk frame: %q", value)
		}
	case 201:
		if !strings.Contains(upper, "TOOL") {
			return fmt.Errorf("Q201 expected TOOL prefix, got %q", value)
		}
	case 303:
		if !strings.Contains(upper, "LAST CYCLE") && !strings.Contains(upper, "PREVIOUS CYCLE") {
			return fmt.Errorf("Q303 expected LAST CYCLE prefix, got %q", value)
		}
	case 402:
		if !strings.Contains(upper, "PARTS") {
			return fmt.Errorf("Q402 expected PARTS prefix, got %q", value)
		}
	case 500:
		if !strings.Contains(upper, "PROGRAM") {
			return fmt.Errorf("Q500 expected PROGRAM prefix, got %q", value)
		}
	case 600:
		if !strings.Contains(upper, "MACRO") {
			return fmt.Errorf("Q600 expected MACRO prefix, got %q", value)
		}
		if macroVar != nil {
			tag := strconv.Itoa(*macroVar)
			// Tolerate either ", N," or ",N," spacing the bridge happens
			// to emit. The MACRO frame is comma-delimited.
			if !strings.Contains(value, ", "+tag+",") && !strings.Contains(value, ","+tag+",") {
				return fmt.Errorf("Q600 macro var mismatch: asked %d, got %q", *macroVar, value)
			}
		}
	}
	return nil
}

// parseValue is best-effort structured parsing — same shape contract as
// the Python dashboard returns. Callers that need more detail should
// look at the raw + value fields.
func parseValue(value string, qCode int, macroVar *int) any {
	if value == "" {
		return nil
	}
	parts := splitAndTrim(value, ",")
	switch {
	case qCode == 500:
		// PROGRAM,<O#>,<status>,PARTS,<n>
		if len(parts) >= 5 &&
			strings.EqualFold(parts[0], "PROGRAM") &&
			strings.EqualFold(parts[3], "PARTS") {
			return map[string]string{
				"program": parts[1],
				"status":  parts[2],
				"parts":   parts[4],
			}
		}
		// fallback pair-based dict
		out := map[string]string{}
		for i := 0; i+1 < len(parts); i += 2 {
			k := strings.ReplaceAll(strings.ToLower(parts[i]), " ", "_")
			out[k] = parts[i+1]
		}
		if len(out) == 0 {
			return nil
		}
		return out
	case qCode == 600 && macroVar != nil:
		// "MACRO, <var>, <value>" — value is usually last; walk backward.
		for i := len(parts) - 1; i >= 0; i-- {
			if v, ok := parseNumber(parts[i]); ok {
				return v
			}
		}
		return nil
	}
	if len(parts) >= 2 {
		if v, ok := parseNumber(parts[len(parts)-1]); ok {
			return v
		}
		return parts[len(parts)-1]
	}
	if len(parts) == 1 {
		return parts[0]
	}
	return nil
}

func parseNumber(s string) (any, bool) {
	if strings.Contains(s, ".") {
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return f, true
		}
		return nil, false
	}
	if i, err := strconv.Atoi(s); err == nil {
		return i, true
	}
	return nil, false
}

func splitAndTrim(s, sep string) []string {
	out := []string{}
	for _, p := range strings.Split(s, sep) {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func sinceMs(t time.Time) float64 {
	return float64(time.Since(t).Microseconds()) / 1000.0
}
