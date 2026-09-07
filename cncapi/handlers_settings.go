package cncapi

// Shared logic behind /api/cnc/machines, /api/cnc/settings, and
// /api/cnc/settings/token. filebrowser's wire shape for settings
// (cncSettingsBody, http/cnc.go) carries legacy single-machine mirror
// fields a pre-multi-machine UI still POSTs — that shape stays in
// http/cnc.go, host-specific. What's shared is the part that doesn't
// care about wire shape: validating a Machines list before it lands
// in storage, minting IDs and tokens, and listing what's configured.

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/filebrowser/filebrowser/v2/settings"
)

// NewMachineID returns a 16-byte URL-safe random machine ID (~22
// chars). Mirrors http/cnc.go's newMachineID, including its
// crypto/rand-failure fallback.
func NewMachineID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		// Crypto-rand failure is exotic; fall back to a timestamp so
		// the install isn't bricked. Collision risk negligible at
		// machine-config cadence.
		return fmt.Sprintf("m%d", time.Now().UnixNano())
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

// NewMachineToken returns a 32-byte URL-safe random bearer token.
// Mirrors http/cnc.go's cncRegenerateTokenHandler's token generation.
func NewMachineToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// NormalizeMachines validates + assigns IDs to a Machines list before
// it lands in storage. Empty list is rejected — the install must
// always have at least one machine. Mirrors http/cnc.go's
// normalizeMachines exactly (byte-for-byte behavior; existing is kept
// as a thin wrapper so http/cnc_serial_only_test.go keeps testing the
// same code path under its original name).
func NormalizeMachines(in []settings.Machine, existing []settings.Machine) ([]settings.Machine, error) {
	if len(in) == 0 {
		return nil, fmt.Errorf("at least one machine required")
	}
	seenIDs := make(map[string]struct{}, len(in))
	out := make([]settings.Machine, 0, len(in))
	for i, m := range in {
		if strings.TrimSpace(m.Name) == "" {
			return nil, fmt.Errorf("machine %d: name required", i)
		}
		// A machine is reached either over TCP (Host:Port, the
		// Waveshare bridge) or over a direct serial device. One of the
		// two is required; both may be set, and Serial.Device wins at
		// dial time.
		if strings.TrimSpace(m.Host) == "" && strings.TrimSpace(m.Serial.Device) == "" {
			return nil, fmt.Errorf("machine %d (%s): host or serial.device required", i, m.Name)
		}
		if m.Port <= 0 {
			m.Port = settings.DefaultHaasPort
		}
		if m.Port > 65535 {
			return nil, fmt.Errorf("machine %d (%s): port out of range", i, m.Name)
		}
		if m.ToolSlots < 0 || m.ToolSlots > 200 {
			return nil, fmt.Errorf("machine %d (%s): toolSlots must be 0..200", i, m.Name)
		}
		if strings.TrimSpace(m.Brand) == "" {
			m.Brand = settings.MachineBrandHaas
		}
		switch m.CameraType {
		case "", "auto", "hls", "mjpeg", "iframe", "none":
			if m.CameraType == "" {
				m.CameraType = "auto"
			}
		default:
			return nil, fmt.Errorf("machine %d (%s): invalid cameraType %q", i, m.Name, m.CameraType)
		}
		// Axes: validate the letters (drop unknowns), canonicalize
		// case. Empty list survives — the consumer treats that as the
		// default X/Y/Z trio.
		if len(m.AxesEnabled) > 0 {
			seen := map[string]bool{}
			allow := map[string]bool{"X": true, "Y": true, "Z": true, "A": true, "B": true, "C": true}
			axesOut := make([]string, 0, len(m.AxesEnabled))
			for _, a := range m.AxesEnabled {
				u := strings.ToUpper(strings.TrimSpace(a))
				if !allow[u] || seen[u] {
					continue
				}
				seen[u] = true
				axesOut = append(axesOut, u)
			}
			m.AxesEnabled = axesOut
		}
		if m.PositionToleranceIn < 0 {
			return nil, fmt.Errorf("machine %d (%s): positionToleranceIn must be >= 0", i, m.Name)
		}
		if m.ID == "" {
			m.ID = NewMachineID()
		}
		if _, dupe := seenIDs[m.ID]; dupe {
			return nil, fmt.Errorf("machine %d (%s): duplicate id %q", i, m.Name, m.ID)
		}
		seenIDs[m.ID] = struct{}{}
		_ = existing // currently unused; kept for future "preserve ID by name match" rules
		out = append(out, m)
	}
	return out, nil
}

// MachinesListBody is the wire shape for GET /api/cnc/machines,
// shared verbatim between hosts (filebrowser's cncMachinesListHandler
// used this exact map shape inline; cncd uses the named type for the
// same JSON).
type MachinesListBody struct {
	Machines  []settings.Machine `json:"machines"`
	DefaultID string             `json:"default_id"`
}

// MachinesList builds the GET /api/cnc/machines body from the live
// registry (not the settings snapshot) so it reflects exactly what's
// wired up, the same as http/cnc.go's cncMachinesListHandler.
func (d Deps) MachinesList() MachinesListBody {
	ms := d.Registry.Machines()
	defaultID := ""
	if len(ms) > 0 {
		defaultID = ms[0].ID
	}
	return MachinesListBody{Machines: ms, DefaultID: defaultID}
}

// SettingsUpdateMachines validates and replaces the Machines list,
// then refreshes the live registry so added/removed machines take
// effect immediately. Shared by filebrowser's multi-machine PUT path
// and cncd's settings PUT.
func (d Deps) SettingsUpdateMachines(in []settings.Machine) error {
	err := d.Store.Update(func(c *settings.Cnc) error {
		cleaned, err := NormalizeMachines(in, c.Machines)
		if err != nil {
			return err
		}
		c.Machines = cleaned
		return nil
	})
	if err != nil {
		return err
	}
	d.Registry.Refresh()
	return nil
}

// RegenerateMachineToken mints a new machine token and persists it.
// Mirrors http/cnc.go's cncRegenerateTokenHandler.
func (d Deps) RegenerateMachineToken() (string, error) {
	tok, err := NewMachineToken()
	if err != nil {
		return "", err
	}
	if err := d.Store.Update(func(c *settings.Cnc) error {
		c.MachineToken = tok
		return nil
	}); err != nil {
		return "", err
	}
	return tok, nil
}
