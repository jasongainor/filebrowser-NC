package cnc

import (
	"strconv"
	"testing"
)

// CAMstudio's tool comment shape uses no `=` between the key and the
// value, and lists fields comma-separated:
//
//	( T1 D0.5 in End mill, TL=6.575, CL=4.0, FL=2.0 )
//
// Pre-existing parser required `D=0.5`; this regression test pins the
// extended regex that accepts both shapes.
func TestPreflightDiamRegexAcceptsCAMstudioShape(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  float64
		ok    bool
	}{
		{"fusion-equals", "T5 D=0.5 CR=0.06 - bullnose", 0.5, true},
		{"fusion-equals-spacing", "T5 D = 0.5", 0.5, true},
		{"camstudio-no-equals", "T1 D0.5 in End mill, TL=6.575, CL=4.0, FL=2.0", 0.5, true},
		{"camstudio-decimal", "T2 D0.25 in Drill", 0.25, true},
		{"camstudio-int-only", "T3 D1 in slot", 1, true},
		{"no-d-word", "T9 grooving tool", 0, false},
		{"d-word-no-digit", "T9 Drill bit", 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := diamRe.FindStringSubmatch(c.input)
			if !c.ok {
				if m != nil {
					t.Fatalf("expected no match in %q, got %v", c.input, m)
				}
				return
			}
			if m == nil {
				t.Fatalf("expected diameter match in %q, got none", c.input)
			}
			v, err := strconv.ParseFloat(m[1], 64)
			if err != nil {
				t.Fatalf("parse %q: %v", m[1], err)
			}
			if v != c.want {
				t.Fatalf("expected %.4f, got %.4f", c.want, v)
			}
		})
	}
}

// CR<num> in the CAMstudio shape — same equals-optional treatment.
func TestPreflightCornerRegexAcceptsCAMstudioShape(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  float64
		ok    bool
	}{
		{"fusion-equals", "T5 D=0.5 CR=0.06 bull", 0.06, true},
		{"camstudio-no-equals", "T6 D0.5 CR0.03 in bull", 0.03, true},
		{"no-cr", "T7 D0.25 in end mill", 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := cornerRe.FindStringSubmatch(c.input)
			if !c.ok {
				if m != nil {
					t.Fatalf("expected no match in %q, got %v", c.input, m)
				}
				return
			}
			if m == nil {
				t.Fatalf("expected corner-radius match in %q, got none", c.input)
			}
			v, err := strconv.ParseFloat(m[1], 64)
			if err != nil {
				t.Fatalf("parse %q: %v", m[1], err)
			}
			if v != c.want {
				t.Fatalf("expected %.4f, got %.4f", c.want, v)
			}
		})
	}
}
