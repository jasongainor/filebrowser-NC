package cncd

// MCP server mount — makes cncd itself an MCP server, in-process,
// over the same streamable-HTTP transport mark3labs/mcp-go provides.
// The tool wiring (machine_state, tool_table, check_tools,
// preflight_program, list_programs, and the program-identity doc
// resource) lives in cncd/mcp; this file is only the seam between
// that transport-agnostic package and cncd's own Deps/Store/Registry,
// plus the HTTP mount and the daemon's existing bearer check.
//
// NewMCPServer is exported (rather than kept as an unexported detail
// of registerMCP) so cmd/cncd's `mcp-stdio` subcommand can build the
// identical tool set over stdio — no HTTP, no bearer, a locally-
// spawned subprocess being its own trust boundary — without
// duplicating any of the Deps-building logic below.

import (
	"encoding/json"
	"net/http"
	"os"

	"github.com/gorilla/mux"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/filebrowser/filebrowser/v2/cnc"
	"github.com/filebrowser/filebrowser/v2/cncapi"
	"github.com/filebrowser/filebrowser/v2/cncd/mcp"
	"github.com/filebrowser/filebrowser/v2/settings"
)

// NewMCPServer builds cncd's MCP server over d. Every collaborator
// mcp.Deps needs — the registry, a root-jailed path resolver, and the
// two reads cncd already knows how to do (find a configured machine,
// load the latest persisted tool-table dump) — is wired here so
// cncd/mcp never has to know cncd's on-disk layout.
func NewMCPServer(d Deps) *mcpserver.MCPServer {
	resolver := cncapi.NewRootResolver(d.Root)
	return mcp.NewServer(mcp.Deps{
		Registry: d.Registry,
		Resolver: resolver,
		Root:     d.Root,
		FindMachine: func(id string) *settings.Machine {
			return findMachine(d.Config.Snapshot(), id)
		},
		LatestToolTable: func(machineID string) (*cnc.ToolTable, error) {
			return latestToolTable(resolver, machineID)
		},
	})
}

// registerMCP mounts the MCP server's streamable-HTTP transport at
// /mcp, gated by the same machine-token bearer every other bearer-
// only cncd route uses (see Deps.bearerOrUnauthorized in router.go).
// Called once from NewRouter.
func registerMCP(r *mux.Router, d Deps) {
	httpServer := mcpserver.NewStreamableHTTPServer(NewMCPServer(d))
	r.PathPrefix("/mcp").Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !d.bearerOrUnauthorized(w, r) {
			return
		}
		httpServer.ServeHTTP(w, r)
	}))
}

// latestToolTable loads the most recently persisted tool-table dump
// for machineID, mirroring the read buildMachineToolList does in
// toollist.go — (nil, nil) when no dump has ever been written.
func latestToolTable(resolver cncapi.PathResolver, machineID string) (*cnc.ToolTable, error) {
	dir, err := toolTableDirAbs(resolver, machineID)
	if err != nil {
		return nil, err
	}
	latest, err := newestJSONIn(dir)
	if err != nil {
		return nil, err
	}
	if latest == "" {
		return nil, nil
	}
	buf, err := os.ReadFile(latest)
	if err != nil {
		return nil, err
	}
	var t cnc.ToolTable
	if err := json.Unmarshal(buf, &t); err != nil {
		return nil, err
	}
	return &t, nil
}
