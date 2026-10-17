package cnc

import "testing"

// The setup sheet reads Q500 straight off the Link, where Parsed is still
// parseValue's map[string]string, not a JSON-decoded map[string]any.
func TestLoadedProgramFromRawQ500(t *testing.T) {
	value := "PROGRAM, O03104, IDLE, PARTS, 15"
	got := LoadedProgramNumber(extractProgram(&Metric{Parsed: parseValue(value, 500, nil), Value: value}))
	if got != "3104" {
		t.Fatalf("loaded program = %q, want 3104", got)
	}
}
