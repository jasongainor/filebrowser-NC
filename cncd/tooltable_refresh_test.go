package cncd

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/filebrowser/filebrowser/v2/cnc"
	"github.com/filebrowser/filebrowser/v2/settings"
)

type fakeRefreshStreamer struct {
	mu      sync.Mutex
	running bool
	linkUp  bool
	tbl     *cnc.ToolTable
	err     error
	reads   int
	slots   int
}

func (f *fakeRefreshStreamer) IsRunning() bool { return f.running }
func (f *fakeRefreshStreamer) LinkUp() bool    { return f.linkUp }
func (f *fakeRefreshStreamer) ReadToolTable(_ context.Context, slots int) (*cnc.ToolTable, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	f.slots = slots
	return f.tbl, f.err
}
func (f *fakeRefreshStreamer) readCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}

func fullTable(readAt time.Time, slots int) *cnc.ToolTable {
	one := 1.0
	t := &cnc.ToolTable{MachineID: "m1", ReadAt: readAt, SlotsRequested: slots, SlotsRead: slots}
	for i := 1; i <= slots; i++ {
		t.Slots = append(t.Slots, cnc.ToolTableSlot{Slot: i, LengthGeom: &one})
	}
	return t
}

func srcOf(f *fakeRefreshStreamer) func(string) refreshStreamer {
	return func(string) refreshStreamer { return f }
}

func TestToolTableRefreshInterval(t *testing.T) {
	cases := []struct {
		minutes int
		want    time.Duration
		ok      bool
	}{
		{0, 30 * time.Minute, true},
		{5, 5 * time.Minute, true},
		{-1, 0, false},
	}
	for _, c := range cases {
		d, ok := settings.Cnc{ToolTableRefreshMinutes: c.minutes}.ToolTableRefreshInterval()
		if d != c.want || ok != c.ok {
			t.Errorf("minutes=%d: got (%v,%v), want (%v,%v)", c.minutes, d, ok, c.want, c.ok)
		}
	}

	deps, _ := newTestDeps(t, nil)
	if !ShouldRefreshToolTable(deps) {
		t.Errorf("unconfigured cncd should refresh")
	}
	deps, _ = newTestDeps(t, func(c *settings.Cnc) { c.ToolTableRefreshMinutes = -1 })
	if ShouldRefreshToolTable(deps) {
		t.Errorf("negative minutes should disable the refresher")
	}
	// Disabled config: the runner returns immediately, never reads.
	f := &fakeRefreshStreamer{linkUp: true, tbl: fullTable(time.Now(), 30)}
	done := make(chan struct{})
	go func() { runToolTableRefresher(context.Background(), deps, 0, srcOf(f)); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("disabled refresher did not return")
	}
}

func TestToolTableRefreshPersistsAndUsesDefaultSlots(t *testing.T) {
	deps, _ := newTestDeps(t, func(c *settings.Cnc) { c.Machines[0].ToolSlots = 20 })
	f := &fakeRefreshStreamer{linkUp: true, tbl: fullTable(time.Now(), 20)}
	refreshToolTables(context.Background(), deps, srcOf(f))
	if f.reads != 1 || f.slots != 20 {
		t.Fatalf("reads=%d slots=%d, want 1 read of 20 slots", f.reads, f.slots)
	}
	got := latest(t, deps)
	if got == nil || got.SlotsRead != 20 {
		t.Fatalf("expected persisted 20-slot table, got %+v", got)
	}
}

// A full read where the controller answered some fields with an empty
// response (the TM-2P does this for diameter_wear on some slots) is
// still persisted; otherwise the refresher would never save on zinc.
func TestToolTableRefreshPersistsFullReadWithFieldErrors(t *testing.T) {
	deps, _ := newTestDeps(t, func(c *settings.Cnc) { c.Machines[0].ToolSlots = 20 })
	tbl := fullTable(time.Now(), 20)
	tbl.Slots[2].Errors = map[string]string{"diameter_wear": "empty response"}
	tbl.Slots[14].Errors = map[string]string{"diameter_wear": "empty response"}
	refreshToolTables(context.Background(), deps, srcOf(&fakeRefreshStreamer{linkUp: true, tbl: tbl}))
	got := latest(t, deps)
	if got == nil || !got.ReadAt.Equal(tbl.ReadAt) {
		t.Fatalf("full read with field errors was not persisted, got %+v", got)
	}
}

func latest(t *testing.T, d Deps) *cnc.ToolTable {
	t.Helper()
	tbl, err := cncapiDepsFor(d).LatestToolTable("m1")
	if err != nil {
		t.Fatalf("LatestToolTable: %v", err)
	}
	return tbl
}

func TestToolTableRefreshSkipsBusyAndDown(t *testing.T) {
	deps, _ := newTestDeps(t, nil)
	for name, f := range map[string]*fakeRefreshStreamer{
		"job active": {running: true, linkUp: true, tbl: fullTable(time.Now(), 30)},
		"link down":  {linkUp: false, tbl: fullTable(time.Now(), 30)},
	} {
		refreshToolTables(context.Background(), deps, srcOf(f))
		if f.reads != 0 {
			t.Errorf("%s: scheduled read ran (reads=%d)", name, f.reads)
		}
	}
	if got := latest(t, deps); got != nil {
		t.Errorf("nothing should be persisted, got %+v", got)
	}
	// The real (offline, unconfigured) streamer is also skipped without error.
	refreshToolTables(context.Background(), deps, registryStreamer(deps))
}

func TestToolTableRefreshKeepsLastGoodOnBadRead(t *testing.T) {
	deps, _ := newTestDeps(t, nil)
	good := fullTable(time.Now().Add(-time.Hour), 30)
	refreshToolTables(context.Background(), deps, srcOf(&fakeRefreshStreamer{linkUp: true, tbl: good}))

	partial := fullTable(time.Now(), 30)
	partial.SlotsRead = 12
	bad := map[string]*fakeRefreshStreamer{
		"error + partial table": {linkUp: true, tbl: partial, err: errors.New("cancelled")},
		"short read":            {linkUp: true, tbl: partial},
		"read failed":           {linkUp: true, err: errors.New("link down")},
	}
	for name, f := range bad {
		refreshToolTables(context.Background(), deps, srcOf(f))
		if f.reads != 1 {
			t.Errorf("%s: reads=%d, want 1", name, f.reads)
		}
		got := latest(t, deps)
		if got == nil || !got.ReadAt.Equal(good.ReadAt) {
			t.Fatalf("%s: last good dump was replaced: %+v", name, got)
		}
	}
}

func TestToolTableRefreshLoopTicksAndStops(t *testing.T) {
	deps, _ := newTestDeps(t, nil)
	f := &fakeRefreshStreamer{linkUp: true, tbl: fullTable(time.Now(), 30)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { runToolTableRefresher(ctx, deps, 10*time.Millisecond, srcOf(f)); close(done) }()
	deadline := time.Now().Add(2 * time.Second)
	for f.readCount() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if f.readCount() < 2 {
		t.Fatalf("expected repeated ticks, reads=%d", f.readCount())
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("refresher did not stop on ctx cancel")
	}
}
