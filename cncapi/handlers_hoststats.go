package cncapi

// Shared logic behind GET /api/cnc/host-stats. Mirrors
// http/cnc_host_stats.go — cnc.ReadHostStats() takes no arguments and
// touches nothing host-specific, so this is a direct passthrough.

import "github.com/filebrowser/filebrowser/v2/cnc"

// HostStats mirrors cncHostStatsHandler.
func HostStats() cnc.HostStats {
	return cnc.ReadHostStats()
}
