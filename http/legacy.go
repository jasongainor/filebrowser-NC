package fbhttp

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
)

// legacyAssets holds the ES5 fallback UI. It is embedded directly rather than
// built by Vite: the whole point of this UI is that it never passes through a
// bundler that would emit ES modules or transform it into something a 2015
// browser cannot parse. See legacy/app.js for the full constraint list.
//
//go:embed legacy
var legacyAssets embed.FS

// legacyHandler serves the browse-only UI used by devices too old to run the
// main application — specifically iOS 9 / Safari 9, whose lack of Proxy makes
// Vue 3 impossible to boot regardless of transpilation.
//
// The assets are static and carry no user data; every request that reads a
// file still goes through the authenticated /api routes.
func legacyHandler(baseURL string) http.Handler {
	// The router runs behind stripPrefix, so the request reaching this handler
	// no longer carries BaseURL and cannot be used to reconstruct it. Build the
	// redirect target up front instead. http.Redirect resolves a relative
	// Location against the (already stripped) request path, which would send a
	// BaseURL install to a path that does not exist.
	redirectTo := path.Join("/", baseURL, "legacy") + "/"

	sub, err := fs.Sub(legacyAssets, "legacy")
	if err != nil {
		// Only reachable if the embed directive above is broken, which is a
		// build-time mistake rather than a runtime condition.
		panic(err)
	}

	files := http.FileServer(http.FS(sub))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// app.js is referenced relatively, so the page only resolves it when
		// served from a path ending in a slash.
		if r.URL.Path == "/legacy" {
			http.Redirect(w, r, redirectTo, http.StatusMovedPermanently)
			return
		}

		// Deliberately not the 1-day max-age used for the hashed bundle
		// assets: these filenames never change, and an operator should not
		// have to clear a decade-old Safari's cache to pick up a fix.
		w.Header().Set("Cache-Control", "no-cache")

		// Strip without the trailing slash so that "/legacy/" maps to "/" and
		// the file server resolves index.html on its own.
		http.StripPrefix("/legacy", files).ServeHTTP(w, r)
	})
}
