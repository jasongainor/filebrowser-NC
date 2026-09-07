package cncd

// The per-machine tool-list engine (used by both the admin-CRUD
// toollist route in router_cnc.go and the firmware endpoint in
// display.go) now lives in cncapi.Deps.BuildMachineToolList, shared
// verbatim with filebrowser's http/cnc_toollist.go. This file keeps
// only the constant cncd's own history-listing route
// (toolTableHistoryHandler, router_cnc.go) needs for its
// display-facing folder prefix.

// toolTableShareDir mirrors http/cnc.go's constant of the same name:
// the root-relative folder tool-table JSON dumps live under.
const toolTableShareDir = "cnc-tool-tables"
