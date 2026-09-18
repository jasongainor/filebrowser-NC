package cnc

import "testing"

// Q402 on an NGC control (Haas TM-2P) answers "M30 #1, N", not the
// classic "PARTS, N". Both must validate; a Q104 mode frame must
// reject either as cross-talk.
func TestValidateResponseShapeQ402Variants(t *testing.T) {
	for _, v := range []string{"PARTS, 12", "M30 #1, 4364", "m30 #2, 7"} {
		if err := validateResponseShape(402, nil, v); err != nil {
			t.Errorf("Q402 %q: unexpected error %v", v, err)
		}
	}
	if err := validateResponseShape(402, nil, "MODE, MEM"); err == nil {
		t.Errorf("Q402 accepted a mode frame")
	}
	if err := validateResponseShape(104, nil, "M30 #1, 4364"); err == nil {
		t.Errorf("Q104 accepted an M30 counter frame as a mode")
	}
	if got := parseValue("M30 #1, 4364", 402, nil); got != 4364 {
		t.Errorf("parseValue Q402 = %v (%T), want 4364", got, got)
	}
}
