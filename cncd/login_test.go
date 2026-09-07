package cncd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/filebrowser/filebrowser/v2/settings"
)

// fakeAuthenticator is the Authenticator test double: no Samba, no
// network, just an in-memory username->password map. It also counts
// calls so rate-limit tests can assert the limiter short-circuits
// before ever reaching the authenticator.
type fakeAuthenticator struct {
	mu    sync.Mutex
	valid map[string]string
	calls int
}

func (f *fakeAuthenticator) Authenticate(_ context.Context, username, password string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if want, ok := f.valid[username]; ok && want == password {
		return nil
	}
	return errors.New("invalid credentials")
}

func (f *fakeAuthenticator) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// fakeClock is the injectable clock login_test.go uses in place of
// time.Now, so TTL-expiry tests don't need a real sleep.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{now: time.Now()} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// newLoginTestRouter builds a cncd router whose login service uses
// authn, clock, and a captured (never actually sleeping) delay
// function instead of talking to real Samba or sleeping for real.
// Returns the router and a pointer to the recorded sleep durations so
// tests can assert the constant-delay behavior fired.
func newLoginTestRouter(t *testing.T, authn Authenticator, clock *fakeClock, ttl time.Duration) (http.Handler, *[]time.Duration) {
	t.Helper()
	deps, _ := newTestDeps(t, nil)

	sleeps := &[]time.Duration{}
	orig := newLoginService
	newLoginService = func(d Deps) *loginService {
		now := clock.Now
		return &loginService{
			Authenticator: authn,
			Sessions:      newSessionStore(now),
			Limiter:       newRateLimiter(now, rateLimitWindow, rateLimitMax),
			Now:           now,
			Sleep:         func(d time.Duration) { *sleeps = append(*sleeps, d) },
			TTL:           ttl,
		}
	}
	t.Cleanup(func() { newLoginService = orig })

	return NewRouter(deps), sleeps
}

func doLogin(r http.Handler, username, password string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(loginRequest{Username: username, Password: password})
	req := httptest.NewRequest(http.MethodPost, "/api/login", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func sessionCookieFrom(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName {
			return c
		}
	}
	return nil
}

func TestLogin_SuccessSetsCookieAndMeWorks(t *testing.T) {
	authn := &fakeAuthenticator{valid: map[string]string{"jason": "correct-horse"}}
	r, _ := newLoginTestRouter(t, authn, newFakeClock(), time.Hour)

	rec := doLogin(r, "jason", "correct-horse")
	if rec.Code != http.StatusOK {
		t.Fatalf("login: got %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	cookie := sessionCookieFrom(rec)
	if cookie == nil {
		t.Fatalf("login: no %s cookie set", sessionCookieName)
	}
	if !cookie.HttpOnly {
		t.Errorf("session cookie: HttpOnly = false, want true")
	}
	if cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("session cookie: SameSite = %v, want Lax", cookie.SameSite)
	}
	var body meResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("login response not valid JSON: %v (%s)", err, rec.Body.String())
	}
	if body.Username != "jason" || !body.Admin || !body.Modify {
		t.Fatalf("login response = %+v, want {jason true true}", body)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/me with cookie: got %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("/api/me response not valid JSON: %v", err)
	}
	if body.Username != "jason" || !body.Admin || !body.Modify {
		t.Fatalf("/api/me response = %+v, want {jason true true}", body)
	}
}

func TestMe_NoCookie_Unauthorized(t *testing.T) {
	authn := &fakeAuthenticator{valid: map[string]string{"jason": "pw"}}
	r, _ := newLoginTestRouter(t, authn, newFakeClock(), time.Hour)

	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", rec.Code)
	}
}

func TestLogin_WrongPassword_UnauthorizedWithDelay(t *testing.T) {
	authn := &fakeAuthenticator{valid: map[string]string{"jason": "correct-horse"}}
	r, sleeps := newLoginTestRouter(t, authn, newFakeClock(), time.Hour)

	rec := doLogin(r, "jason", "wrong")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password: got %d, want 401", rec.Code)
	}
	if sessionCookieFrom(rec) != nil {
		t.Fatalf("wrong password: a session cookie was set")
	}
	if len(*sleeps) != 1 || (*sleeps)[0] != loginFailureDelay {
		t.Fatalf("sleeps = %v, want exactly one call of %v", *sleeps, loginFailureDelay)
	}
}

func TestLogin_UnknownUser_SameFailureShape(t *testing.T) {
	authn := &fakeAuthenticator{valid: map[string]string{"jason": "correct-horse"}}
	r, sleeps := newLoginTestRouter(t, authn, newFakeClock(), time.Hour)

	rec := doLogin(r, "nobody", "whatever")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown user: got %d, want 401", rec.Code)
	}
	if len(*sleeps) != 1 {
		t.Fatalf("sleeps = %v, want exactly one delayed failure", *sleeps)
	}
}

func TestLogin_RateLimitTripsAfterFiveFailures(t *testing.T) {
	authn := &fakeAuthenticator{valid: map[string]string{"jason": "correct-horse"}}
	r, _ := newLoginTestRouter(t, authn, newFakeClock(), time.Hour)

	for i := 0; i < rateLimitMax; i++ {
		rec := doLogin(r, "jason", "wrong")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("failure %d: got %d, want 401", i+1, rec.Code)
		}
	}
	if got := authn.callCount(); got != rateLimitMax {
		t.Fatalf("authenticator calls = %d, want %d before the limit trips", got, rateLimitMax)
	}

	// The 6th attempt, even with the *correct* password, should be
	// rejected by the limiter before it ever reaches the authenticator.
	rec := doLogin(r, "jason", "correct-horse")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("6th attempt: got %d, want 429", rec.Code)
	}
	if got := authn.callCount(); got != rateLimitMax {
		t.Fatalf("authenticator calls after limit trip = %d, want still %d (limiter should short-circuit)", got, rateLimitMax)
	}
}

func TestLogin_RateLimitIsPerIP(t *testing.T) {
	authn := &fakeAuthenticator{valid: map[string]string{"jason": "correct-horse"}}
	r, _ := newLoginTestRouter(t, authn, newFakeClock(), time.Hour)

	for i := 0; i < rateLimitMax; i++ {
		body, _ := json.Marshal(loginRequest{Username: "jason", Password: "wrong"})
		req := httptest.NewRequest(http.MethodPost, "/api/login", bytes.NewReader(body))
		req.RemoteAddr = "10.0.0.1:5555"
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("failure %d from 10.0.0.1: got %d, want 401", i+1, rec.Code)
		}
	}

	// A different remote IP should not be affected by 10.0.0.1's limit.
	body, _ := json.Marshal(loginRequest{Username: "jason", Password: "correct-horse"})
	req := httptest.NewRequest(http.MethodPost, "/api/login", bytes.NewReader(body))
	req.RemoteAddr = "10.0.0.2:6666"
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("different IP: got %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
}

func TestLogout_InvalidatesSession(t *testing.T) {
	authn := &fakeAuthenticator{valid: map[string]string{"jason": "correct-horse"}}
	r, _ := newLoginTestRouter(t, authn, newFakeClock(), time.Hour)

	rec := doLogin(r, "jason", "correct-horse")
	cookie := sessionCookieFrom(rec)
	if cookie == nil {
		t.Fatalf("login: no cookie set")
	}

	req := httptest.NewRequest(http.MethodPost, "/api/logout", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("logout: got %d, want 200", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/me", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("/api/me after logout: got %d, want 401", rec.Code)
	}
}

func TestSession_ExpiredRejected(t *testing.T) {
	authn := &fakeAuthenticator{valid: map[string]string{"jason": "correct-horse"}}
	clock := newFakeClock()
	r, _ := newLoginTestRouter(t, authn, clock, time.Hour)

	rec := doLogin(r, "jason", "correct-horse")
	cookie := sessionCookieFrom(rec)
	if cookie == nil {
		t.Fatalf("login: no cookie set")
	}

	clock.Advance(time.Hour + time.Minute)

	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("/api/me after expiry: got %d, want 401", rec.Code)
	}
}

func TestFiles_SessionAllowsPutDeniesWithout(t *testing.T) {
	authn := &fakeAuthenticator{valid: map[string]string{"jason": "correct-horse"}}
	r, _ := newLoginTestRouter(t, authn, newFakeClock(), time.Hour)

	// No session, no bearer: forbidden.
	req := httptest.NewRequest(http.MethodPut, "/api/files?path=/part.nc", bytes.NewBufferString("G0 X0\n"))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("PUT without session: got %d, want 403", rec.Code)
	}

	loginRec := doLogin(r, "jason", "correct-horse")
	cookie := sessionCookieFrom(loginRec)
	if cookie == nil {
		t.Fatalf("login: no cookie set")
	}

	req = httptest.NewRequest(http.MethodPut, "/api/files?path=/part.nc", bytes.NewBufferString("G0 X0\n"))
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT with session: got %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
}

// TestBearerPath_StillWorksAlongsideSessions guards against the
// session-cookie fallback in authzFor accidentally shadowing or
// breaking the machine-token bearer path — the two are meant to be
// independent "or" conditions.
func TestBearerPath_StillWorksAlongsideSessions(t *testing.T) {
	t.Helper()
	deps, _ := newTestDeps(t, func(c *settings.Cnc) {
		c.MachineToken = "s3cret"
	})

	authn := &fakeAuthenticator{valid: map[string]string{"jason": "correct-horse"}}
	orig := newLoginService
	newLoginService = func(d Deps) *loginService {
		now := time.Now
		return &loginService{
			Authenticator: authn,
			Sessions:      newSessionStore(now),
			Limiter:       newRateLimiter(now, rateLimitWindow, rateLimitMax),
			Now:           now,
			Sleep:         func(time.Duration) {},
			TTL:           time.Hour,
		}
	}
	t.Cleanup(func() { newLoginService = orig })

	r := NewRouter(deps)

	// Bearer, no cookie at all: still succeeds.
	req := httptest.NewRequest(http.MethodPut, "/api/files?path=/part.nc", bytes.NewBufferString("G0 X0\n"))
	req.Header.Set("Authorization", "Bearer s3cret")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT with bearer (no session): got %d, want 200 (%s)", rec.Code, rec.Body.String())
	}

	// Neither bearer nor session: forbidden.
	req = httptest.NewRequest(http.MethodPut, "/api/files?path=/part2.nc", bytes.NewBufferString("G0 X0\n"))
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("PUT with neither: got %d, want 403", rec.Code)
	}
}
