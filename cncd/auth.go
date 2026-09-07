package cncd

import (
	"net/http"
	"strings"

	"github.com/filebrowser/filebrowser/v2/cncapi"
)

// extractBearer pulls the token out of `Authorization: Bearer <t>`,
// returning "" when the header is missing or malformed. Mirrors
// fbhttp's extractBearer (http/cnc_displays.go) so the two behave
// identically for clients that talk to either.
func extractBearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h == "" {
		return ""
	}
	parts := strings.SplitN(h, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return strings.TrimSpace(parts[1])
}

// authzFor decides the caller's Authz for a request. There are two
// independent ways to prove you're more than a read-only LAN visitor:
// present the machine token as a bearer (unchanged — the m2m path
// renishaw-builder and friends use), or carry a valid session cookie
// from POST /api/login (see login.go), which is how the web UI proves
// a human signed in against Samba. Either grants admin+modify;
// neither present is read-only. Individual routes that have no
// read-only mode of their own (e.g. /api/cnc/state, which has no
// session fallback here the way it does in filebrowser) enforce the
// bearer themselves rather than relying on this being admin-gated.
func authzFor(r *http.Request, machineToken string) cncapi.Authz {
	if machineToken != "" && extractBearer(r) == machineToken {
		return cncapi.StaticAuthz{Modify: true, Admin: true}
	}
	if a, ok := sessionAuthz(r); ok {
		return a
	}
	return cncapi.StaticAuthz{}
}

// sessionAuthz checks r's session cookie against the process's
// currently active login service (see login.go's "Active login
// service registry"). A valid, unexpired session is treated as full
// admin+modify — cncd is a single-operator box with exactly one
// Samba-authenticated identity, not a multi-user system, so there is
// no group or role to narrow it to yet.
//
// TODO(multi-operator): once more than one Samba user is expected to
// sign in, map group membership (or a per-user allowlist) to a
// narrower Authz here instead of this blanket grant.
func sessionAuthz(r *http.Request) (cncapi.Authz, bool) {
	svc := getActiveLogin()
	if svc == nil {
		return cncapi.StaticAuthz{}, false
	}
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return cncapi.StaticAuthz{}, false
	}
	if _, ok := svc.Sessions.Lookup(cookie.Value); !ok {
		return cncapi.StaticAuthz{}, false
	}
	return cncapi.StaticAuthz{Modify: true, Admin: true}, true
}
