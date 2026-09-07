package fbhttp

// The CNC handlers (cnc*.go in this package) resolve paths and check
// permissions through the two small interfaces in cncapi, not through
// *users.User directly. That indirection is what lets the same
// handler logic run inside filebrowser (backed by the requesting
// user's scope) or inside cmd/cncd (backed by a bare root directory,
// no users at all). See docs/REPO_SPLIT_TODO.md.
//
// This file provides the filebrowser-side adapters: userPathResolver
// and userAuthz wrap *users.User to satisfy cncapi.PathResolver and
// cncapi.Authz, so behaviour for existing (authenticated) requests is
// byte-for-byte unchanged.

import (
	"github.com/filebrowser/filebrowser/v2/cnc"
	"github.com/filebrowser/filebrowser/v2/cncapi"
	"github.com/filebrowser/filebrowser/v2/settings"
	"github.com/filebrowser/filebrowser/v2/users"
)

// userPathResolver adapts *users.User to cncapi.PathResolver.
// users.User.FullPath never errors today (it's a plain afero path
// join against the user's already-validated scope), so the adapter
// always returns a nil error; the error return exists for resolvers
// that can fail, like cncapi.RootResolver.
type userPathResolver struct {
	u *users.User
}

func (p userPathResolver) FullPath(rel string) (string, error) {
	return p.u.FullPath(rel), nil
}

// userAuthz adapts *users.User to cncapi.Authz.
type userAuthz struct {
	u *users.User
}

func (a userAuthz) CanModify() bool { return a.u.Perm.Modify }
func (a userAuthz) IsAdmin() bool   { return a.u.Perm.Admin }

// pathResolver returns the seam through which cnc handlers resolve
// CNC-relative paths. When d.user is hydrated (the normal
// authenticated path, via withUser/withAdmin) it's backed by that
// user's scope. When it isn't — the sole case left is the
// unauthenticated firmware endpoint, GET /api/displays/{id} — it
// falls back to a resolver rooted at the server root, which is
// exactly the scope an admin user resolves to (admins carry an empty
// Scope, so their Fs base is the server root; see users.User.Clean).
// That replaces the old firstAdminUser hack, which borrowed a real
// admin's *users.User just to reach FullPath.
func (d *data) pathResolver() cncapi.PathResolver {
	if d.user != nil {
		return userPathResolver{u: d.user}
	}
	return cncapi.NewRootResolver(d.server.Root)
}

// authz returns the seam through which cnc handlers check
// permissions. Mirrors pathResolver's fallback: the unauthenticated
// firmware endpoint only ever reads, so an admin+modify default for
// the no-user case is harmless (nothing in that handler consults it
// today) and keeps the two seams symmetric.
func (d *data) authz() cncapi.Authz {
	if d.user != nil {
		return userAuthz{u: d.user}
	}
	return cncapi.StaticAuthz{Modify: true, Admin: true}
}

// settingsStore adapts *data to cncapi.SettingsStore: Snapshot reads
// the in-memory settings.Cnc already hydrated onto d by handle()
// (http/data.go), and Update mutates that same document in place
// before persisting the *whole* settings.Settings document through
// d.store.Settings.Save — filebrowser has no narrower "just the Cnc
// sub-document" save path, so a shared handler that only touches
// .Cnc still gets everything else round-tripped for free.
type settingsStore struct {
	d *data
}

func (s settingsStore) Snapshot() settings.Cnc { return s.d.settings.Cnc }

func (s settingsStore) Update(fn func(*settings.Cnc) error) error {
	if err := fn(&s.d.settings.Cnc); err != nil {
		return err
	}
	return s.d.store.Settings.Save(s.d.settings)
}

// cncapiDeps builds the cncapi.Deps a shared handler needs from this
// request's *data plus the live registry (which isn't reachable from
// *data itself — every http/cnc*.go handler closes over it via its
// constructor, e.g. cncStartHandler(registry *cnc.Registry)).
func (d *data) cncapiDeps(registry *cnc.Registry) cncapi.Deps {
	return cncapi.Deps{
		Registry: registry,
		Resolver: d.pathResolver(),
		Authz:    d.authz(),
		Store:    settingsStore{d: d},
	}
}
