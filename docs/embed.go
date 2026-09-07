// Package docs embeds selected repository documentation so it can
// ship inside a compiled binary without the doc file living next to
// it on disk. cncd runs headless on the shop Pi with no checked-out
// repo around it — see docs/CNCD.md — so its MCP server (cncd/mcp)
// can't just os.ReadFile("docs/PROGRAM_IDENTITY.md") the way a repo-
// local tool could. Embedding at compile time means the doc travels
// with the binary and a CAM session can read the sidecar contract
// straight from the running daemon.
package docs

import _ "embed"

// ProgramIdentityMD is the verbatim contents of PROGRAM_IDENTITY.md:
// the GMW-* header format, the .gmw.json sidecar JSON Schema, and the
// Fusion post-processor snippet that writes both. Exposed as an MCP
// resource by cncd/mcp so a Fusion-side CAM session can pull the
// sidecar contract directly from the box it's about to send a program
// to, rather than needing a copy of this repo.
//
//go:embed PROGRAM_IDENTITY.md
var ProgramIdentityMD string
