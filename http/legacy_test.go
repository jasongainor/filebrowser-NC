package fbhttp

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func legacyFile(t *testing.T, name string) string {
	t.Helper()

	b, err := fs.ReadFile(legacyAssets, "legacy/"+name)
	if err != nil {
		t.Fatalf("reading embedded legacy/%s: %v", name, err)
	}
	return string(b)
}

func TestLegacyHandlerRoutes(t *testing.T) {
	t.Parallel()

	h := legacyHandler("")

	testCases := map[string]struct {
		method       string
		path         string
		expectStatus int
		expectType   string
		expectBody   string
	}{
		// Relative asset references only resolve from a directory URL, so the
		// bare path has to redirect rather than render.
		"bare path redirects": {
			method:       http.MethodGet,
			path:         "/legacy",
			expectStatus: http.StatusMovedPermanently,
		},
		"directory serves the page": {
			method:       http.MethodGet,
			path:         "/legacy/",
			expectStatus: http.StatusOK,
			expectType:   "text/html",
			expectBody:   "<title>File Browser</title>",
		},
		"script is served": {
			method:       http.MethodGet,
			path:         "/legacy/app.js",
			expectStatus: http.StatusOK,
			expectType:   "javascript",
		},
		"writes are rejected": {
			method:       http.MethodPost,
			path:         "/legacy/",
			expectStatus: http.StatusMethodNotAllowed,
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))

			if w.Code != tc.expectStatus {
				t.Errorf("status = %d, want %d", w.Code, tc.expectStatus)
			}
			if tc.expectType != "" && !strings.Contains(w.Header().Get("Content-Type"), tc.expectType) {
				t.Errorf("Content-Type = %q, want it to contain %q", w.Header().Get("Content-Type"), tc.expectType)
			}
			if tc.expectBody != "" && !strings.Contains(w.Body.String(), tc.expectBody) {
				t.Errorf("body does not contain %q", tc.expectBody)
			}
		})
	}
}

// TestLegacyRedirectHonoursBaseURL guards the BaseURL case. The router sits
// behind stripPrefix, so by the time this handler runs the request path has
// already lost the prefix — a redirect built from the request would drop it
// too and land on a path that does not exist.
func TestLegacyRedirectHonoursBaseURL(t *testing.T) {
	t.Parallel()

	testCases := map[string]struct {
		baseURL string
		expect  string
	}{
		"no base url":       {baseURL: "", expect: "/legacy/"},
		"base url":          {baseURL: "/fb", expect: "/fb/legacy/"},
		"nested base url":   {baseURL: "/tools/fb", expect: "/tools/fb/legacy/"},
		"base url no slash": {baseURL: "fb", expect: "/fb/legacy/"},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			w := httptest.NewRecorder()
			// stripPrefix has already removed the base, so the handler always
			// sees the bare "/legacy".
			legacyHandler(tc.baseURL).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/legacy", nil))

			if got := w.Header().Get("Location"); got != tc.expect {
				t.Errorf("Location = %q, want %q", got, tc.expect)
			}
		})
	}
}

// TestLegacyScriptIsES5 is the guardrail for this whole UI. app.js exists to
// serve browsers that predate ES6 — Safari 9 on an iPad 3 is the reason it was
// written — and every construct below parses as a syntax error there, which
// takes down the entire page rather than degrading. Modernising this file
// silently defeats its only purpose, so the constraint is enforced here.
func TestLegacyScriptIsES5(t *testing.T) {
	t.Parallel()

	src := stripJSComments(legacyFile(t, "app.js"))

	banned := []struct {
		name    string
		pattern *regexp.Regexp
	}{
		{"arrow function", regexp.MustCompile(`=>`)},
		{"let declaration", regexp.MustCompile(`\blet\s`)},
		{"const declaration", regexp.MustCompile(`\bconst\s`)},
		{"template literal", regexp.MustCompile("`")},
		{"class declaration", regexp.MustCompile(`\bclass\s+\w`)},
		{"fetch()", regexp.MustCompile(`\bfetch\s*\(`)},
		{"Promise", regexp.MustCompile(`\bPromise\b`)},
		{"async/await", regexp.MustCompile(`\basync\s|\bawait\s`)},
		{"Object.assign", regexp.MustCompile(`\bObject\.assign\b`)},
		{"Array.from", regexp.MustCompile(`\bArray\.from\b`)},
		{"String.includes/startsWith/endsWith", regexp.MustCompile(`\.(includes|startsWith|endsWith)\s*\(`)},
		{"spread/rest", regexp.MustCompile(`\.\.\.`)},
		{"import/export", regexp.MustCompile(`^\s*(import|export)\s`)},
	}

	for _, b := range banned {
		if loc := b.pattern.FindStringIndex(src); loc != nil {
			line := strings.Count(src[:loc[0]], "\n") + 1
			t.Errorf("app.js line %d uses %s, which Safari 9 cannot parse", line, b.name)
		}
	}
}

// TestLegacyPageMatchesCSP pins the asset layout to the Content-Security-Policy
// set in NewHandler: `default-src 'self'; style-src 'unsafe-inline';`.
//
// Because style-src is stated explicitly it does not inherit 'self', so an
// external stylesheet is blocked and CSS must be inline. script-src is absent
// and falls back to default-src 'self', so the inverse holds for JavaScript.
// Getting this backwards produces a page that serves 200 and renders unstyled
// or dead, which is easy to miss.
func TestLegacyPageMatchesCSP(t *testing.T) {
	t.Parallel()

	page := legacyFile(t, "index.html")

	if regexp.MustCompile(`<link[^>]+rel=["']stylesheet`).MatchString(page) {
		t.Error("index.html links an external stylesheet, which style-src 'unsafe-inline' blocks; inline the CSS instead")
	}
	if !strings.Contains(page, "<style>") {
		t.Error("index.html has no inline <style> block")
	}
	if !regexp.MustCompile(`<script[^>]+src=`).MatchString(page) {
		t.Error("index.html does not load app.js as an external script")
	}
	// Note the [^<\s] terminator: an empty <script src=…></script> must not
	// count, and plain \S would match the "<" that opens the closing tag.
	if regexp.MustCompile(`<script(?:\s[^>]*)?>[^<]*[^<\s]`).MatchString(page) {
		t.Error("index.html contains an inline script, which default-src 'self' blocks")
	}
	if regexp.MustCompile(`\son(click|submit|load|change)\s*=`).MatchString(page) {
		t.Error("index.html uses an inline event handler attribute, which default-src 'self' blocks")
	}
	// type="module" is the specific thing that makes the main bundle a no-op on
	// iOS 9: Safari does not recognise it and never fetches the script at all.
	if strings.Contains(page, `type="module"`) {
		t.Error(`index.html uses type="module", which Safari 9 silently ignores`)
	}
	if !strings.Contains(page, `name="viewport"`) {
		t.Error("index.html has no viewport meta; iOS inflates text without it")
	}
}

// stripJSComments blanks out comments so that prose describing the forbidden
// constructs does not trip the scan that forbids them.
func stripJSComments(src string) string {
	var out strings.Builder
	out.Grow(len(src))

	const (
		code = iota
		lineComment
		blockComment
		str
	)

	state := code
	var quote byte

	for i := 0; i < len(src); i++ {
		c := src[i]

		switch state {
		case code:
			switch {
			case c == '/' && i+1 < len(src) && src[i+1] == '/':
				state = lineComment
				i++
			case c == '/' && i+1 < len(src) && src[i+1] == '*':
				state = blockComment
				i++
			case c == '"' || c == '\'':
				state = str
				quote = c
				out.WriteByte(c)
			default:
				out.WriteByte(c)
			}
		case lineComment:
			if c == '\n' {
				state = code
				out.WriteByte(c)
			}
		case blockComment:
			if c == '*' && i+1 < len(src) && src[i+1] == '/' {
				state = code
				i++
			} else if c == '\n' {
				out.WriteByte(c) // keep line numbers honest
			}
		case str:
			if c == '\\' && i+1 < len(src) {
				out.WriteByte(c)
				i++
				out.WriteByte(src[i])
				continue
			}
			if c == quote {
				state = code
			}
			out.WriteByte(c)
		}
	}

	return out.String()
}
