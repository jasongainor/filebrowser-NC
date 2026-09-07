package mcp

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	mcpsdk "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/filebrowser/filebrowser/v2/cnc"
)

// ListProgramsArgs is list_programs' input.
type ListProgramsArgs struct {
	Path string `json:"path,omitempty" jsonschema_description:"Subdirectory to scan, relative to the served root. Defaults to the root."`
}

// ProgramEntry is one file found under the share, with its identity
// badge when the file carries a GMW header (cnc.ParseIdentity).
type ProgramEntry struct {
	Path    string    `json:"path"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time"`

	// HasIdentity is false for an un-annotated program — every field
	// below is empty in that case, not an error.
	HasIdentity bool   `json:"has_identity"`
	Job         string `json:"job,omitempty"`
	Operation   string `json:"operation,omitempty"`
	Part        string `json:"part,omitempty"`
	// SHAStatus is "match", "mismatch", or "unstamped" — see
	// cnc.Identity.SHAStatus. Only meaningful when HasIdentity.
	SHAStatus string `json:"sha_status,omitempty"`
	PostedAt  string `json:"posted_at,omitempty"`
}

// ListProgramsResult is list_programs' output.
type ListProgramsResult struct {
	Root     string         `json:"root"`
	Programs []ProgramEntry `json:"programs"`
}

// listProgramsMaxScanBytes bounds how large a file this tool will
// read into memory to check for a GMW header. Actual NC programs run
// well under this; anything larger is almost certainly not a program
// this daemon would preflight, and skipping the read keeps a stray
// large file from blowing up memory on the Pi. The file is still
// listed — just without identity badges.
const listProgramsMaxScanBytes = 8 * 1024 * 1024

// listProgramsSkipDir is the tool-table dump directory cncd writes
// persisted ToolTable JSON into (mirrors cncd/toollist.go's
// toolTableShareDir — duplicated as a literal rather than imported,
// since cncd imports this package and not the other way around).
// Dumps aren't programs; skip the whole subtree.
const listProgramsSkipDir = "cnc-tool-tables"

// sidecarSuffix is the .gmw.json sidecar extension (cnc.SidecarPath).
// Sidecars carry their own identity data already reachable through
// the NC file they sit next to; listing them as separate "programs"
// would just double every entry.
const sidecarSuffix = ".gmw.json"

func registerListPrograms(s *server.MCPServer, d Deps) {
	tool := mcpsdk.NewTool("list_programs",
		mcpsdk.WithDescription(
			"List files on the share, each with an identity badge (job/operation/part/sha_status) "+
				"when the file carries a GMW header written by the Fusion post. Un-annotated files "+
				"are listed too, with has_identity=false — that's normal for anything not run through "+
				"the GMW post yet, not an error."),
		mcpsdk.WithInputSchema[ListProgramsArgs](),
		mcpsdk.WithReadOnlyHintAnnotation(true),
	)
	s.AddTool(tool, listProgramsHandler(d))
}

func listProgramsHandler(d Deps) server.ToolHandlerFunc {
	return func(_ context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		var args ListProgramsArgs
		if err := req.BindArguments(&args); err != nil {
			return mcpsdk.NewToolResultError(err.Error()), nil
		}
		if d.Resolver == nil || d.Root == "" {
			return mcpsdk.NewToolResultError("cncd: no served root configured"), nil
		}

		rel := args.Path
		if rel == "" {
			rel = "/"
		}
		absDir, err := d.Resolver.FullPath(rel)
		if err != nil {
			return mcpsdk.NewToolResultError(err.Error()), nil
		}
		info, err := os.Stat(absDir)
		if err != nil {
			return mcpsdk.NewToolResultError(err.Error()), nil
		}
		if !info.IsDir() {
			return mcpsdk.NewToolResultError(fmt.Sprintf("list_programs: %q is not a directory", rel)), nil
		}

		var entries []ProgramEntry
		walkErr := filepath.WalkDir(absDir, func(p string, de fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if de.IsDir() {
				if de.Name() == listProgramsSkipDir {
					return filepath.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(de.Name(), sidecarSuffix) {
				return nil
			}
			fi, ferr := de.Info()
			if ferr != nil {
				return nil //nolint:nilerr // best-effort listing; skip unreadable entries
			}
			relPath, rerr := filepath.Rel(d.Root, p)
			if rerr != nil {
				relPath = p
			}
			entry := ProgramEntry{
				Path:    filepath.ToSlash(relPath),
				Size:    fi.Size(),
				ModTime: fi.ModTime(),
			}
			if fi.Size() > 0 && fi.Size() <= listProgramsMaxScanBytes {
				if buf, rerr := os.ReadFile(p); rerr == nil {
					if id, ok := cnc.ParseIdentity(buf); ok {
						entry.HasIdentity = true
						entry.Job = id.Job
						entry.Operation = id.Operation
						entry.Part = id.Part
						entry.SHAStatus = id.SHAStatus
						if !id.PostedAt.IsZero() {
							entry.PostedAt = id.PostedAt.Format(time.RFC3339)
						}
					}
				}
			}
			entries = append(entries, entry)
			return nil
		})
		if walkErr != nil {
			return mcpsdk.NewToolResultError(walkErr.Error()), nil
		}

		sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })

		result := ListProgramsResult{Root: rel, Programs: entries}
		identified := 0
		for _, e := range entries {
			if e.HasIdentity {
				identified++
			}
		}
		summary := fmt.Sprintf("%d file(s) under %q, %d with a GMW identity header.", len(entries), rel, identified)
		return mcpsdk.NewToolResultStructured(result, summary), nil
	}
}
