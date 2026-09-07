// Package mcp implements cncd's Model Context Protocol server: the
// box itself becomes an MCP server, exposing the same read-only CNC
// surface the HTTP API already serves (machine state, the live tool
// table, program preflight, sidecar tool reconciliation, and a
// program listing) as five MCP tools plus one resource. The audience
// is a Fusion-side CAM session (via an MCP-driven client) that wants
// to know, before ever sending a program: what tools are actually in
// the machine right now, at what offsets, and whether this program's
// tool list matches them — a swap or a too-short tool caught here
// costs nothing; caught at the machine it costs a crash.
//
// This package knows nothing about cncd's HTTP router, mux, or its
// bearer-token auth — Deps is a narrow, transport-agnostic seam
// (mirrors the split cncapi.PathResolver/Authz already draw between
// cnc/ and its two hosts) so the same tool wiring serves two
// transports without duplication:
//
//   - cncd/router_mcp.go mounts NewServer's streamable-HTTP transport
//     at /mcp, behind the daemon's existing machine-token bearer check.
//   - cmd/cncd's `mcp-stdio` subcommand serves the identical tool set
//     over stdio, with no HTTP and therefore no bearer involved at all
//     — a locally-spawned subprocess (the normal way an MCP client
//     like Claude Desktop or Fusion's own client talks to a local
//     server) is its own trust boundary.
//
// See docs/MCP.md for the tool list, example calls, and what a Fusion
// session should do before posting.
package mcp

import (
	"github.com/mark3labs/mcp-go/server"

	"github.com/filebrowser/filebrowser/v2/cnc"
	"github.com/filebrowser/filebrowser/v2/cncapi"
	"github.com/filebrowser/filebrowser/v2/settings"
)

// serverVersion is the MCP server's reported implementation version
// (part of the initialize handshake). Bump when the tool surface
// changes shape in a way a client might care about.
const serverVersion = "0.1.0"

// Deps is what the MCP tool handlers need from whatever process hosts
// them. Every field is read-only from this package's point of view —
// none of these tools mutate machine state, settings, or the share.
type Deps struct {
	// Registry is the live per-machine Streamer+Aggregator set. Never
	// nil in a real daemon; tools that need it report a clear error
	// when it is (defensive, mainly for hand-built test Deps).
	Registry *cnc.Registry

	// Resolver turns an NC path relative to the served root into an
	// absolute filesystem path, jailed to that root — the same
	// contract cncd's own /api/files uses (cncapi.RootResolver).
	Resolver cncapi.PathResolver

	// Root is the absolute directory Resolver is jailed to. Needed
	// directly by list_programs, which walks the whole tree rather
	// than resolving one caller-supplied path.
	Root string

	// FindMachine returns the settings.Machine for id, the default
	// (first configured) machine when id is "", or nil when nothing
	// matches / no machine is configured at all. Mirrors cncd's
	// unexported findMachine (cncd/toollist.go) — duplicated as a
	// closure here rather than exported, so this package never needs
	// to import cncd (which imports this package).
	FindMachine func(id string) *settings.Machine

	// LatestToolTable loads the most recently persisted tool-table
	// dump for machineID, returning (nil, nil) when no dump has ever
	// been written for that machine. Mirrors the read
	// buildMachineToolList does in cncd/toollist.go.
	LatestToolTable func(machineID string) (*cnc.ToolTable, error)
}

// NewServer builds cncd's MCP server: five read-only tools
// (machine_state, tool_table, check_tools, preflight_program,
// list_programs) plus the program-identity doc resource. Safe to call
// more than once against the same Deps — e.g. once for the HTTP
// mount and once for the `mcp-stdio` subcommand — each call returns
// an independent *server.MCPServer with its own session state.
func NewServer(d Deps) *server.MCPServer {
	s := server.NewMCPServer(
		"cncd",
		serverVersion,
		server.WithToolCapabilities(false),
		server.WithResourceCapabilities(true, false),
	)
	registerMachineState(s, d)
	registerToolTable(s, d)
	registerCheckTools(s, d)
	registerPreflightProgram(s, d)
	registerListPrograms(s, d)
	registerResources(s, d)
	return s
}
