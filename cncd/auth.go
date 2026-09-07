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

// authzFor decides the caller's Authz for a request. cncd has no
// login yet (see docs/CNCD.md) so there is exactly one way to prove
// you're more than a read-only LAN visitor: present the machine
// token as a bearer. That gets admin+modify; anything else — no
// header, wrong token — is read-only, matching the "otherwise
// read-only" rule in the daemon's design brief. Individual routes
// that have no read-only mode of their own (e.g. /api/cnc/state,
// which has no session fallback here the way it does in filebrowser)
// enforce the bearer themselves rather than relying on this being
// admin-gated.
func authzFor(r *http.Request, machineToken string) cncapi.Authz {
	if machineToken == "" {
		return cncapi.StaticAuthz{}
	}
	if extractBearer(r) == machineToken {
		return cncapi.StaticAuthz{Modify: true, Admin: true}
	}
	return cncapi.StaticAuthz{}
}
