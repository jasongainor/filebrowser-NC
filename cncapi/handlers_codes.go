package cncapi

// Shared logic behind /api/cnc/codes/lookup and /api/cnc/codes/search.
// Mirrors http/cnc_codes.go exactly — these two handlers never touched
// a *data at all beyond the withUser wrapper, so there's nothing host
// specific to seam through; they're free functions, not Deps methods.

import "github.com/filebrowser/filebrowser/v2/cnc"

// CodeLookupBody is the wire shape for GET /api/cnc/codes/lookup.
type CodeLookupBody struct {
	OK    bool          `json:"ok"`
	Kind  string        `json:"kind"`
	Entry cnc.CodeEntry `json:"entry"`
}

// CodeLookup mirrors cncCodesLookupHandler.
func CodeLookup(kindRaw string, number int) CodeLookupBody {
	kind := cnc.NormalizeKind(kindRaw)
	entry, ok := cnc.LookupCode(kind, number)
	return CodeLookupBody{OK: ok, Kind: string(kind), Entry: entry}
}

// CodeSearchBody is the wire shape for GET /api/cnc/codes/search.
type CodeSearchBody struct {
	Count   int             `json:"count"`
	Results []cnc.CodeEntry `json:"results"`
}

// CodeSearch mirrors cncCodesSearchHandler. limit is clamped to
// (0, 200]; 0 (unset) defaults to 50.
func CodeSearch(kindRaw, q string, limit int) CodeSearchBody {
	var kind cnc.CodeKind
	if kindRaw != "" {
		kind = cnc.NormalizeKind(kindRaw)
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	results := cnc.SearchCodes(kind, q, limit)
	return CodeSearchBody{Count: len(results), Results: results}
}
