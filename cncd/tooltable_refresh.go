package cncd

// Scheduled tool-table refresh. Nothing else in cncd reads the tool
// table except a manual POST /api/cnc/tool-table, so a display that
// serves GET /api/cnc/tool-table shows whatever the last manual read
// left. This loop re-reads each machine on an interval and persists
// the result — through the same Streamer.ReadToolTable path and the
// same cncapi.Deps.PersistToolTable the POST handler uses, so the RS-232
// line keeps its single owner (the Link) and no second connection is
// opened.

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/filebrowser/filebrowser/v2/cnc"
	"github.com/filebrowser/filebrowser/v2/cncapi"
)

// toolTableRefreshReadTimeout bounds one scheduled read. Same ceiling
// the POST handler uses for a fully-loaded toolchanger at low baud.
const toolTableRefreshReadTimeout = 15 * time.Minute

// refreshStreamer is the slice of *cnc.Streamer the refresher needs;
// a seam so tests can run without a live bridge.
type refreshStreamer interface {
	IsRunning() bool
	LinkUp() bool
	ReadToolTable(ctx context.Context, slots int) (*cnc.ToolTable, error)
}

// streamerAdapter adds LinkUp to *cnc.Streamer from its Link's state.
type streamerAdapter struct{ *cnc.Streamer }

func (a streamerAdapter) LinkUp() bool { return a.Link().State().Up }

func registryStreamer(d Deps) func(id string) refreshStreamer {
	return func(id string) refreshStreamer {
		st, _ := d.Registry.Streamer(id)
		if st == nil {
			return nil
		}
		return streamerAdapter{st}
	}
}

// cncapiDepsFor is the non-request analog of Deps.cncapiDeps: no
// Authz, because the refresher is the daemon itself, not a caller.
func cncapiDepsFor(d Deps) cncapi.Deps {
	return cncapi.Deps{
		Registry: d.Registry,
		Resolver: cncapi.NewRootResolver(d.Root),
		Store:    d.Config,
	}
}

// ShouldRefreshToolTable reports whether the refresher should run for
// the current config (false when ToolTableRefreshMinutes is negative).
// cmd/cncd checks this once at startup; changing the setting takes a
// restart, like every other cncd background loop.
func ShouldRefreshToolTable(d Deps) bool {
	_, ok := d.Config.Snapshot().ToolTableRefreshInterval()
	return ok
}

// RunToolTableRefresher reads and persists every configured machine's
// tool table every interval (<= 0 uses the configured interval, default
// 30 minutes) until ctx is done. The first read happens one interval
// after start, not at boot. Intended to run in its own goroutine for the
// life of the daemon (see cmd/cncd/main.go).
func RunToolTableRefresher(ctx context.Context, d Deps, interval time.Duration) {
	runToolTableRefresher(ctx, d, interval, registryStreamer(d))
}

func runToolTableRefresher(ctx context.Context, d Deps, interval time.Duration, src func(id string) refreshStreamer) {
	if interval <= 0 {
		var ok bool
		if interval, ok = d.Config.Snapshot().ToolTableRefreshInterval(); !ok {
			return
		}
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			refreshToolTables(ctx, d, src)
		}
	}
}

// refreshToolTables does one pass over every configured machine.
func refreshToolTables(ctx context.Context, d Deps, src func(id string) refreshStreamer) {
	cd := cncapiDepsFor(d)
	for _, m := range d.Registry.Machines() {
		if ctx.Err() != nil {
			return
		}
		st := src(m.ID)
		if st == nil {
			continue
		}
		if st.IsRunning() {
			log.Printf("cncd: tool-table refresh %s: skipped, streaming job active", m.ID)
			continue
		}
		if !st.LinkUp() {
			log.Printf("cncd: tool-table refresh %s: skipped, link down", m.ID)
			continue
		}
		slots := cncapi.DefaultToolSlotsForMachine(d.Config.Snapshot(), m.ID)
		rctx, cancel := context.WithTimeout(ctx, toolTableRefreshReadTimeout)
		tbl, err := st.ReadToolTable(rctx, slots)
		cancel()
		if err := toolTableReadComplete(tbl, err); err != nil {
			log.Printf("cncd: tool-table refresh %s: not persisted, keeping last good dump: %v", m.ID, err)
			continue
		}
		if err := cd.PersistToolTable(m.ID, tbl); err != nil {
			log.Printf("cncd: tool-table refresh %s: persist: %v", m.ID, err)
			continue
		}
		log.Printf("cncd: tool-table refresh %s: persisted %d/%d slots", m.ID, tbl.SlotsRead, tbl.SlotsRequested)
	}
}

// toolTableReadComplete returns nil for a full read: no read error and
// every requested slot read. A partial table is fine for an
// operator-triggered POST (it is persisted there), but a scheduled one
// must not replace the last good dump. Per-field slot errors do not
// count against a read: the TM-2P routinely answers some fields with an
// empty response (diameter_wear on slots 3 and 15, 2026-10-11), so
// refusing them would mean no scheduled read is ever persisted.
func toolTableReadComplete(tbl *cnc.ToolTable, readErr error) error {
	if readErr != nil {
		return readErr
	}
	if tbl == nil {
		return fmt.Errorf("no table returned")
	}
	if tbl.SlotsRead < tbl.SlotsRequested {
		return fmt.Errorf("partial read: %d/%d slots", tbl.SlotsRead, tbl.SlotsRequested)
	}
	return nil
}
