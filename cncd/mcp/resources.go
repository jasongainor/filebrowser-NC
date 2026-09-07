package mcp

import (
	"context"

	mcpsdk "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/filebrowser/filebrowser/v2/docs"
)

// programIdentityResourceURI identifies the embedded PROGRAM_IDENTITY.md
// doc as an MCP resource. Not a real fetchable URL — MCP resource URIs
// only need to be unique within the server, and every existing
// resource in the ecosystem uses a scheme like this for content that
// lives inside the server rather than on a filesystem or the web.
const programIdentityResourceURI = "docs://cncd/program-identity"

// registerResources adds the program-identity doc resource — the
// .gmw.json sidecar contract a CAM session needs before it can build
// useful check_tools / preflight_program calls.
func registerResources(s *server.MCPServer, _ Deps) {
	resource := mcpsdk.NewResource(
		programIdentityResourceURI,
		"GMW program identity — header + sidecar contract",
		mcpsdk.WithResourceDescription(
			"The GMW-* NC header format and the .gmw.json sidecar JSON Schema a Fusion post must "+
				"write so cncd can reconcile a program's tools before it ever reaches the machine. "+
				"Read this before wiring up check_tools or relying on preflight_program's identity "+
				"section."),
		mcpsdk.WithMIMEType("text/markdown"),
	)
	s.AddResource(resource, func(_ context.Context, _ mcpsdk.ReadResourceRequest) ([]mcpsdk.ResourceContents, error) {
		return []mcpsdk.ResourceContents{
			mcpsdk.TextResourceContents{
				URI:      programIdentityResourceURI,
				MIMEType: "text/markdown",
				Text:     docs.ProgramIdentityMD,
			},
		}, nil
	})
}
