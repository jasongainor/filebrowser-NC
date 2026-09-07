package fbhttp

import (
	"testing"

	"github.com/filebrowser/filebrowser/v2/settings"
)

// A Pi with a USB-serial cable has no Host at all (PR #140): the
// settings API must accept a serial-only machine and still reject one
// with neither transport.
func TestNormalizeMachinesAcceptsSerialOnly(t *testing.T) {
	in := []settings.Machine{{Name: "TM-2P", Serial: settings.MachineSerial{Device: "/dev/ttyUSB0"}}}
	out, err := normalizeMachines(in, nil)
	if err != nil {
		t.Fatalf("serial-only machine rejected: %v", err)
	}
	if len(out) != 1 || out[0].Serial.Device != "/dev/ttyUSB0" || out[0].ID == "" {
		t.Fatalf("unexpected normalized machine: %+v", out)
	}
}

func TestNormalizeMachinesRejectsNoTransport(t *testing.T) {
	_, err := normalizeMachines([]settings.Machine{{Name: "TM-2P"}}, nil)
	if err == nil {
		t.Fatal("machine with neither host nor serial.device must be rejected")
	}
}
