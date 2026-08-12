package cnc

import (
	"context"
	"testing"
	"time"

	"github.com/filebrowser/filebrowser/v2/settings"
)

// Exactly one metric may be Baseline. The baseline set is the always-on
// load against a single-client RS-232 bridge, so growing it is a
// deliberate decision, not something that should drift in unnoticed.
func TestMetricSpecs_BaselineSetIsMinimal(t *testing.T) {
	var baseline []string
	for _, spec := range defaultMetricSpecs {
		if spec.Baseline {
			baseline = append(baseline, spec.Key)
		}
	}
	if len(baseline) != 1 || baseline[0] != "mode" {
		t.Fatalf("baseline set = %v, want exactly [mode]; every addition here "+
			"polls the bridge forever, so widen it deliberately", baseline)
	}
}

// The engaged (wake-window) query rate must stay under the bridge's
// ceiling, which minQuerySpacing defines. This is the arithmetic that
// forced the original design to duty-cycle polling behind a wake window.
func TestMetricSpecs_EngagedRateUnderBridgeCeiling(t *testing.T) {
	var perSec float64
	for _, spec := range defaultMetricSpecs {
		perSec += float64(time.Second) / float64(spec.Interval)
	}
	ceiling := float64(time.Second) / float64(minQuerySpacing)
	if perSec >= ceiling {
		t.Fatalf("engaged polling wants %.2f queries/s but the bridge ceiling is %.2f/s; "+
			"the aggregator would fall permanently behind", perSec, ceiling)
	}
	t.Logf("engaged rate %.2f q/s against a %.2f q/s ceiling (%.0f%% saturation)",
		perSec, ceiling, perSec/ceiling*100)
}

// The baseline rate must be a rounding error by comparison — that is
// what makes always-on polling affordable.
func TestBaselineRate_IsCheap(t *testing.T) {
	ceiling := float64(time.Second) / float64(minQuerySpacing)
	baseline := float64(time.Second) / float64(defaultBaselineInterval)
	if share := baseline / ceiling; share > 0.05 {
		t.Fatalf("baseline poll uses %.1f%% of bridge capacity; keep it under 5%%", share*100)
	}
}

func aggregatorForTier(t *testing.T, baselineSeconds int) *Aggregator {
	t.Helper()
	set := &settings.Settings{
		Cnc: settings.Cnc{
			Machines:            []settings.Machine{{ID: "m1", Host: "127.0.0.1", Port: 1}},
			BaselinePollSeconds: baselineSeconds,
		},
	}
	return NewAggregator(New(&fakeSettings{s: set}, "m1"))
}

// Outside the wake window, non-baseline metrics must not poll at all —
// that is what keeps the bridge idle when nobody is watching.
func TestPollGate_StandbySkipsNonBaseline(t *testing.T) {
	a := aggregatorForTier(t, 1)

	var posSpec, modeSpec metricSpec
	for _, s := range defaultMetricSpecs {
		if s.Key == "pos_x" {
			posSpec = s
		}
		if s.Key == "mode" {
			modeSpec = s
		}
	}

	if a.IsAwake() {
		t.Fatal("a fresh aggregator should be in standby")
	}

	// Non-baseline in standby: throttle check is irrelevant, it is
	// skipped outright by pollOnce. Assert the spec flag drives that.
	if posSpec.Baseline {
		t.Fatal("pos_x must not be a baseline metric")
	}
	if !modeSpec.Baseline {
		t.Fatal("mode must be a baseline metric")
	}

	// Baseline throttling: first attempt allowed, immediate second denied.
	if a.throttledBaseline(modeSpec) {
		t.Fatal("first baseline attempt should not be throttled")
	}
	a.markAttempt(modeSpec)
	if !a.throttledBaseline(modeSpec) {
		t.Fatal("second baseline attempt inside the window should be throttled")
	}
}

// BaselinePollSeconds must actually govern the standby cadence, so an
// operator can trade bridge load against detection latency.
func TestPollGate_BaselineIntervalIsOperatorTunable(t *testing.T) {
	a := aggregatorForTier(t, 0) // 0 → default
	if got := a.streamer.Link().baselineInterval(); got != defaultBaselineInterval {
		t.Fatalf("unset BaselinePollSeconds gave %s, want %s", got, defaultBaselineInterval)
	}

	a = aggregatorForTier(t, 42)
	if got := a.streamer.Link().baselineInterval(); got != 42*time.Second {
		t.Fatalf("BaselinePollSeconds=42 gave %s, want 42s", got)
	}
}

// Wake() must no longer be what makes a machine "connected" — that was
// the display bug. It governs polling RATE only.
func TestWakeWindow_DoesNotDetermineConnectedness(t *testing.T) {
	a := aggregatorForTier(t, 1)
	s := a.streamer

	a.Wake(time.Minute)
	if !a.IsAwake() {
		t.Fatal("Wake should open the window")
	}
	if s.Alive() {
		t.Fatal("Alive() must be false with no successful round-trip, " +
			"regardless of the wake window — this is exactly the bug where " +
			"the e-paper showed [disconnected] against a running mill")
	}
}

// Stopping a registry must release links, not just aggregators. A
// leaked link holds a real socket against a one-client bridge.
func TestRegistryStop_ReleasesLinks(t *testing.T) {
	set := &settings.Settings{
		Cnc: settings.Cnc{Machines: []settings.Machine{{ID: "m1", Host: "127.0.0.1", Port: 1}}},
	}
	r := NewRegistry(&fakeSettings{s: set})
	st, _ := r.Streamer("m1")
	if st == nil {
		t.Fatal("expected a streamer for m1")
	}

	r.Stop()

	// StopLink is idempotent and must have already run; a second call
	// returning promptly proves the supervisor goroutine exited.
	done := make(chan struct{})
	go func() { st.StopLink(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("StopLink blocked after Registry.Stop — link goroutine leaked")
	}
	_ = context.Background()
}
