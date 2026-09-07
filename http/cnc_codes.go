package fbhttp

// /api/cnc/codes/* — Haas alarm/setting/parameter code lookup.
// Lets the UI translate a bare number like "Setting 414" into a
// human-readable explanation. Read-only; no registry interaction.
//
// Logic lives in cncapi.CodeLookup / cncapi.CodeSearch, shared with
// cncd — these never touched a *data at all beyond the withUser
// wrapper, so there's nothing host-specific left here but the wrapper
// itself.

import (
	"net/http"
	"strconv"

	"github.com/filebrowser/filebrowser/v2/cncapi"
)

// GET /api/cnc/codes/lookup?kind=setting&number=414
var cncCodesLookupHandler = withUser(func(w http.ResponseWriter, r *http.Request, _ *data) (int, error) {
	n, err := strconv.Atoi(r.URL.Query().Get("number"))
	if err != nil {
		return http.StatusBadRequest, err
	}
	return renderJSON(w, r, cncapi.CodeLookup(r.URL.Query().Get("kind"), n))
})

// GET /api/cnc/codes/search?q=probe&kind=setting&limit=20
var cncCodesSearchHandler = withUser(func(w http.ResponseWriter, r *http.Request, _ *data) (int, error) {
	limit := 0
	if l := r.URL.Query().Get("limit"); l != "" {
		if v, err := strconv.Atoi(l); err == nil {
			limit = v
		}
	}
	return renderJSON(w, r, cncapi.CodeSearch(r.URL.Query().Get("kind"), r.URL.Query().Get("q"), limit))
})
