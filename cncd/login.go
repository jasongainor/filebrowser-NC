// login.go is cncd's sign-in path: "Smb settings can be the auth
// instead of a sign in screen" (the daemon's design brief, see
// docs/CNCD.md's "Sign-in" section). There is no second user store —
// a POST /api/login validates the given username/password by
// performing an SMB2 session setup against the Samba server already
// running on the Pi (the same one that maps the share the Haas
// mounts), and on success issues an opaque session cookie. No
// password is ever logged, and none is kept beyond the SMB
// round-trip that validates it.
package cncd

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	smb2 "github.com/hirochachacha/go-smb2"
)

const (
	// sessionCookieName is the HttpOnly cookie carrying the opaque
	// session id. Its value is never anything an attacker could
	// derive from the username or password — see randomSessionID.
	sessionCookieName = "cncd_session"

	// defaultSMBAddress is where cncd looks for the Samba server when
	// Auth.SMBAddress is unset: the Pi's own smbd, which is the only
	// configuration this daemon targets (see docs/CNCD.md).
	defaultSMBAddress = "127.0.0.1:445"

	// defaultSessionTTL is used when Auth.SessionTTLHours is unset —
	// a shop-floor kiosk shouldn't demand re-login every shift change.
	defaultSessionTTL = 24 * 7 * time.Hour

	// smbDialTimeout bounds how long a login attempt waits on the
	// local Samba server before failing. Loopback should answer in
	// milliseconds; this is generous headroom, not a real budget.
	smbDialTimeout = 5 * time.Second

	// loginFailureDelay is added to every failed login response
	// (wrong password, unknown user, rate-limited) so timing can't
	// distinguish "no such user" from "wrong password", and so
	// brute-forcing costs wall-clock time even before the rate
	// limiter engages. Applied via loginService.Sleep so tests can
	// inject a no-op.
	loginFailureDelay = 300 * time.Millisecond

	// rateLimitWindow / rateLimitMax implement "5 failures per minute
	// per remote IP."
	rateLimitWindow = time.Minute
	rateLimitMax    = 5
)

// Authenticator checks a username/password pair against whatever
// backs cncd's identity. The production implementation (smbAuthenticator)
// talks to Samba; tests inject a fake so they never dial a real
// network service.
type Authenticator interface {
	Authenticate(ctx context.Context, username, password string) error
}

// smbAuthenticator is the production Authenticator: it validates
// credentials by performing an SMB2 session setup (NTLM) against the
// configured Samba server and immediately logging off. It never
// mounts a share or reads a file — establishing the session is
// authentication enough, exactly as smbclient/net use would be.
type smbAuthenticator struct {
	Config *Store
}

// Authenticate dials Config's Auth.SMBAddress (default 127.0.0.1:445),
// negotiates an SMB2 session as username/password under Auth.Domain,
// and logs off. A successful session setup is the only signal used —
// no share is mounted. The password never leaves this function: it is
// handed to go-smb2's NTLMInitiator by value and is not retained,
// logged, or included in any returned error.
func (a *smbAuthenticator) Authenticate(ctx context.Context, username, password string) error {
	cfg := a.Config.Snapshot().Auth
	addr := cfg.SMBAddress
	if addr == "" {
		addr = defaultSMBAddress
	}

	dialer := &net.Dialer{Timeout: smbDialTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("cncd: dial smb server: %w", err)
	}
	defer conn.Close()

	d := &smb2.Dialer{
		Initiator: &smb2.NTLMInitiator{
			User:     username,
			Password: password,
			Domain:   cfg.Domain,
		},
	}
	sess, err := d.DialContext(ctx, conn)
	if err != nil {
		return fmt.Errorf("cncd: smb session setup failed: %w", err)
	}
	defer sess.Logoff()

	return nil
}

// newAuthenticator builds the Authenticator registerLogin wires up
// for a real daemon. It's a package var (not a plain function) so
// tests can swap in a fake Authenticator without cncd/router.go's
// Deps struct growing an extra field just for that — Deps is shared
// with every other route in the package, and this change is scoped
// to login.go/router_login.go plus a two-line change to auth.go.
var newAuthenticator = func(cfg *Store) Authenticator {
	return &smbAuthenticator{Config: cfg}
}

// sessionRecord is one issued session: who it belongs to, and when it
// stops being valid.
type sessionRecord struct {
	username string
	expires  time.Time
}

// sessionStore is an in-memory, TTL-expiring set of session ids. It
// is intentionally not persisted — a daemon restart invalidates every
// session, which is the right failure mode for a box with no
// meaningful "remember me" requirement beyond a browser tab staying
// open across a shift.
type sessionStore struct {
	now func() time.Time

	mu   sync.Mutex
	byID map[string]sessionRecord
}

func newSessionStore(now func() time.Time) *sessionStore {
	return &sessionStore{now: now, byID: make(map[string]sessionRecord)}
}

// Create mints a new random session id for username, valid for ttl,
// and stores it.
func (s *sessionStore) Create(username string, ttl time.Duration) (string, error) {
	id, err := randomSessionID()
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byID[id] = sessionRecord{username: username, expires: s.now().Add(ttl)}
	return id, nil
}

// Lookup reports whether id is a currently-valid session, returning
// its record. An expired session is evicted on lookup rather than
// left to leak memory forever, and is reported as not found.
func (s *sessionStore) Lookup(id string) (sessionRecord, bool) {
	if id == "" {
		return sessionRecord{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.byID[id]
	if !ok {
		return sessionRecord{}, false
	}
	if !rec.expires.After(s.now()) {
		delete(s.byID, id)
		return sessionRecord{}, false
	}
	return rec, true
}

// Delete invalidates id, if present. Deleting an unknown id is a
// no-op — logout doesn't care whether the cookie it was handed was
// already stale.
func (s *sessionStore) Delete(id string) {
	if id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.byID, id)
}

// randomSessionID returns a URL-safe, unguessable session id: 32
// bytes of crypto/rand, base64-encoded.
func randomSessionID() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("cncd: generate session id: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// rateLimiter enforces "N failures per window per key" — cncd uses it
// for "5 failures per minute per remote IP" on POST /api/login. Only
// failures count against the limit; a successful login never adds to
// it.
type rateLimiter struct {
	now    func() time.Time
	window time.Duration
	limit  int

	mu       sync.Mutex
	failures map[string][]time.Time
}

func newRateLimiter(now func() time.Time, window time.Duration, limit int) *rateLimiter {
	return &rateLimiter{now: now, window: window, limit: limit, failures: make(map[string][]time.Time)}
}

// Allowed reports whether key (a remote IP) is currently under the
// limit. It does not itself record anything — call RecordFailure
// separately once the attempt is known to have failed.
func (rl *rateLimiter) Allowed(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	return len(rl.prune(key)) < rl.limit
}

// RecordFailure adds a failure timestamp for key.
func (rl *rateLimiter) RecordFailure(key string) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.failures[key] = append(rl.prune(key), rl.now())
}

// prune drops timestamps for key older than the window and returns
// (and stores) the survivors. Caller must hold rl.mu.
func (rl *rateLimiter) prune(key string) []time.Time {
	cutoff := rl.now().Add(-rl.window)
	fs := rl.failures[key]
	kept := fs[:0:0]
	for _, t := range fs {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	rl.failures[key] = kept
	return kept
}

// loginService bundles everything a login/logout/me handler needs.
// Time and sleeping are injected (Now/Sleep) so tests can verify the
// constant-delay and rate-limit-window behavior without a real clock
// or a real 300ms sleep per failed attempt.
type loginService struct {
	Authenticator Authenticator
	Sessions      *sessionStore
	Limiter       *rateLimiter
	Now           func() time.Time
	Sleep         func(time.Duration)
	TTL           time.Duration
}

// newLoginService builds the loginService registerLogin mounts. It's
// a package var, like newAuthenticator, so tests can override the
// whole thing — swapping in a fake Authenticator and a controllable
// clock — without NewRouter or Deps changing shape.
var newLoginService = func(d Deps) *loginService {
	ttlHours := d.Config.Snapshot().Auth.SessionTTLHours
	ttl := defaultSessionTTL
	if ttlHours > 0 {
		ttl = time.Duration(ttlHours) * time.Hour
	}
	now := time.Now
	return &loginService{
		Authenticator: newAuthenticator(d.Config),
		Sessions:      newSessionStore(now),
		Limiter:       newRateLimiter(now, rateLimitWindow, rateLimitMax),
		Now:           now,
		Sleep:         time.Sleep,
		TTL:           ttl,
	}
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type meResponse struct {
	Username string `json:"username"`
	Admin    bool   `json:"admin"`
	Modify   bool   `json:"modify"`
}

// handleLogin serves POST /api/login. See the package doc comment for
// the shape of the exchange; failures (bad body, unknown user, wrong
// password, rate-limited) are all reported identically as a 401 after
// a constant delay, so a network observer learns nothing beyond
// "that attempt failed."
func (s *loginService) handleLogin(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)

	if !s.Limiter.Allowed(ip) {
		writeError(w, http.StatusTooManyRequests, nil)
		return
	}

	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Username == "" || req.Password == "" {
		s.fail(w, ip)
		return
	}

	if err := s.Authenticator.Authenticate(r.Context(), req.Username, req.Password); err != nil {
		s.fail(w, ip)
		return
	}

	id, err := s.Sessions.Create(req.Username, s.TTL)
	if err != nil {
		writeError(w, http.StatusInternalServerError, nil)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    id,
		Path:     "/",
		Expires:  s.Now().Add(s.TTL),
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
	})
	_ = renderJSON(w, meResponse{Username: req.Username, Admin: true, Modify: true})
}

// fail records a rate-limit failure, applies the constant delay, and
// writes 401. Centralized so every failure path (bad body, bad
// credentials) behaves identically.
func (s *loginService) fail(w http.ResponseWriter, ip string) {
	s.Limiter.RecordFailure(ip)
	s.Sleep(loginFailureDelay)
	writeError(w, http.StatusUnauthorized, nil)
}

// handleLogout serves POST /api/logout: invalidates the session (if
// any) and clears the cookie. Always succeeds — logging out of a
// session that's already gone isn't an error.
func (s *loginService) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookieName); err == nil {
		s.Sessions.Delete(c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
	})
	w.WriteHeader(http.StatusOK)
}

// handleMe serves GET /api/me: reports the current session's identity,
// or 401 when there is none / it has expired.
func (s *loginService) handleMe(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		writeError(w, http.StatusUnauthorized, nil)
		return
	}
	rec, ok := s.Sessions.Lookup(c.Value)
	if !ok {
		writeError(w, http.StatusUnauthorized, nil)
		return
	}
	_ = renderJSON(w, meResponse{Username: rec.username, Admin: true, Modify: true})
}

// clientIP extracts the remote IP (no port) for rate-limiting
// purposes. cncd has no reverse proxy in its deployment (see
// docs/CNCD.md) so RemoteAddr is trustworthy as-is; this deliberately
// does not honor X-Forwarded-For, which would let a LAN client spoof
// its way around the rate limit.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return strings.TrimSpace(r.RemoteAddr)
	}
	return host
}

// ── Active login service registry ──
//
// authzFor (auth.go) needs to check the caller's session cookie, but
// its signature — authzFor(r, machineToken) — is shared with every
// other route in the package and isn't the place to thread a new
// dependency through. Instead, registerLogin publishes the
// loginService it built as "the" active one for this process, and
// authzFor reads it back. In production there is exactly one
// NewRouter call per process, so this is just a slightly indirect
// field; in tests, each test builds its own Deps/router and this is
// overwritten accordingly (tests run sequentially, never in
// parallel, within this package).
var (
	activeLoginMu sync.RWMutex
	activeLogin   *loginService
)

func setActiveLogin(s *loginService) {
	activeLoginMu.Lock()
	activeLogin = s
	activeLoginMu.Unlock()
}

func getActiveLogin() *loginService {
	activeLoginMu.RLock()
	defer activeLoginMu.RUnlock()
	return activeLogin
}
