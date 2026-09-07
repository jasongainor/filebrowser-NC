// Package cncapi holds the seam between the CNC handlers (cnc/,
// http/cnc*.go) and whatever hosts them. Historically that host was
// always filebrowser's *users.User — path resolution went through
// u.FullPath and permission checks through u.Perm.Modify / u.Perm.Admin.
// That coupling is what made cnc/ and http/cnc*.go impossible to run
// without a filebrowser install (bolt DB, user accounts, Vue frontend)
// behind them.
//
// PathResolver and Authz are the two capabilities the CNC handlers
// actually need from a host. filebrowser satisfies them with a thin
// adapter over *users.User (see http/cnc_seam.go); cmd/cncd, which
// has no users at all, satisfies them with RootResolver and a bearer
// check against the configured machine token. See docs/REPO_SPLIT_TODO.md
// for why this seam exists.
package cncapi

import (
	"fmt"
	"path/filepath"
	"strings"
)

// PathResolver turns a CNC-relative path (e.g. "/jobs/part.nc") into
// an absolute filesystem path. Implementations are expected to jail
// the result to whatever scope they represent — a filebrowser user's
// scoped share, or (for cncd) the single directory the daemon serves.
type PathResolver interface {
	FullPath(rel string) (string, error)
}

// Authz reports what the current caller may do. The CNC handlers only
// ever needed these two questions answered — never a finer-grained
// permission model.
type Authz interface {
	// CanModify reports whether the caller may perform mutating
	// actions: start/stop a job, edit a tool table, manage the queue.
	CanModify() bool
	// IsAdmin reports whether the caller may perform admin-only
	// actions: manage displays, regenerate the machine token, change
	// CNC settings.
	IsAdmin() bool
}

// RootResolver is a PathResolver jailed to a single directory. It is
// the resolver cmd/cncd uses for its one served directory, and it
// replaces filebrowser's "borrow an admin user just to call FullPath"
// hack for the unauthenticated firmware endpoint (see the former
// firstAdminUser in http/cnc_displays.go) — a root-jailed join needs
// no user at all.
type RootResolver struct {
	root string
}

// NewRootResolver builds a RootResolver rooted at root. root is
// cleaned once up front so every FullPath call compares against a
// single canonical prefix.
func NewRootResolver(root string) RootResolver {
	return RootResolver{root: filepath.Clean(root)}
}

// FullPath joins rel onto the root and verifies the result is still
// inside the root. rel is always treated as relative — a leading "/"
// does not escape the jail — and any ".." (or combination of the two)
// that would resolve outside root is rejected with an error rather
// than silently clamped back to root. Silent clamping would make an
// escape attempt indistinguishable from a request for the root itself,
// which is the wrong failure mode for a network-facing file API.
func (r RootResolver) FullPath(rel string) (string, error) {
	full := filepath.Clean(filepath.Join(r.root, rel))
	if full != r.root && !strings.HasPrefix(full, r.root+string(filepath.Separator)) {
		return "", fmt.Errorf("cncapi: path %q escapes root %q", rel, r.root)
	}
	return full, nil
}

// StaticAuthz is an Authz with a fixed answer, for callers that have
// already made the modify/admin decision some other way — a bearer
// token match, in cncd's case.
type StaticAuthz struct {
	Modify bool
	Admin  bool
}

// CanModify implements Authz.
func (a StaticAuthz) CanModify() bool { return a.Modify }

// IsAdmin implements Authz.
func (a StaticAuthz) IsAdmin() bool { return a.Admin }
