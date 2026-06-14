package settings

import "testing"

func f64ptr(v float64) *float64 { return &v }

func TestEffectiveReservedTools(t *testing.T) {
	m := Machine{ReservedTools: []ReservedTool{
		{Pocket: 1, Kind: ReservedKindGauge, ExpectedDia: f64ptr(0.5011), ExpectedLen: f64ptr(5.119)},
		{Pocket: 20, Kind: ReservedKindWorkProbe, Tol: 0.005},
	}}
	eff := m.EffectiveReservedTools()
	if len(eff) != 2 {
		t.Fatalf("len = %d, want 2", len(eff))
	}
	if eff[0].Tol != DefaultReservedTol {
		t.Errorf("gauge Tol = %v, want default %v", eff[0].Tol, DefaultReservedTol)
	}
	if eff[1].Tol != 0.005 {
		t.Errorf("explicit Tol = %v, want 0.005", eff[1].Tol)
	}
	if m.ReservedTools[0].Tol != 0 {
		t.Errorf("EffectiveReservedTools mutated the stored slice")
	}
}

func TestReservedToolAt(t *testing.T) {
	m := Machine{ReservedTools: []ReservedTool{
		{Pocket: 1, Kind: ReservedKindGauge},
		{Pocket: 20, Kind: ReservedKindWorkProbe},
	}}
	if r, ok := m.ReservedToolAt(1); !ok || r.Kind != ReservedKindGauge || r.Tol != DefaultReservedTol {
		t.Errorf("pocket 1: %+v ok=%v", r, ok)
	}
	if _, ok := m.ReservedToolAt(5); ok {
		t.Error("pocket 5 should not be reserved")
	}
}

func TestEffectiveReservedToolsEmpty(t *testing.T) {
	var m Machine
	if r := m.EffectiveReservedTools(); r != nil {
		t.Errorf("empty machine should return nil, got %v", r)
	}
}
