package cnc

import (
	"time"
	_ "time/tzdata" // embed the zone database: the Pi image may not carry /usr/share/zoneinfo
)

// The shop runs on Eastern time. Timestamps are stored and sent in UTC;
// only strings meant for a human to read on a display go through here.
var displayLoc = func() *time.Location {
	if l, err := time.LoadLocation("America/New_York"); err == nil {
		return l
	}
	return time.FixedZone("EST", -5*3600)
}()

// DisplayTime formats t for a screen, in America/New_York.
func DisplayTime(t time.Time) string {
	return t.In(displayLoc).Format("2006-01-02 15:04")
}
