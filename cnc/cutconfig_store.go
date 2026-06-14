package cnc

// CutConfigStore holds the parsed cut config in memory and re-parses ONLY
// when the source changes. The ToolList rebuilds on every fetch, but the cut
// config must not — the archive is multi-megabyte (the Harvey library alone
// is ~19.5 MB), so re-reading it per /toollist request would be absurd.
//
// The source is behind an adapter so it can later be a Toolpath API instead
// of a local file. On a reload failure the store keeps serving the last good
// parse and marks it stale, rather than going empty.

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// CutConfigSource is where a cut config comes from. Token() is a cheap
// change-detection probe (e.g. file mtime+size, or an API etag); Load() does
// the expensive parse. A nil config with a nil error means "none configured"
// (a neutral state, not an error).
type CutConfigSource interface {
	Token() string
	Load() (*CutConfig, error)
}

// FileCutConfigSource loads a .cutconfig ZIP from a local path. An empty Path
// means no cut config is configured (Token=="" , Load returns nil,nil).
type FileCutConfigSource struct{ Path string }

func (s FileCutConfigSource) Token() string {
	if s.Path == "" {
		return ""
	}
	st, err := os.Stat(s.Path)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d:%d", st.ModTime().UnixNano(), st.Size())
}

func (s FileCutConfigSource) Load() (*CutConfig, error) {
	if s.Path == "" {
		return nil, nil
	}
	f, err := os.Open(s.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // not configured yet — neutral, not an error
		}
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	return ParseCutConfig(f, st.Size())
}

// CutConfigStore caches the parsed config. Safe for concurrent use.
type CutConfigStore struct {
	src CutConfigSource

	mu       sync.RWMutex
	cfg      *CutConfig
	token    string
	loadedAt time.Time
	stale    bool // a reload failed; we're serving an older parse
	lastErr  string
}

// NewCutConfigStore builds the store and does the initial parse. A parse
// failure is not fatal — Current() returns (nil,false) and reports the error
// via LastError until the source becomes readable.
func NewCutConfigStore(src CutConfigSource) *CutConfigStore {
	s := &CutConfigStore{src: src}
	s.reload()
	return s
}

// Current returns the active cut config (nil when none configured / not yet
// parseable) and whether it is stale (last reload failed). Cheap: it re-parses
// only when the source's change token differs from the loaded one.
func (s *CutConfigStore) Current() (*CutConfig, bool) {
	if s == nil || s.src == nil {
		return nil, false
	}
	s.mu.RLock()
	upToDate := s.token == s.src.Token() && (s.cfg != nil || s.lastErr == "")
	if upToDate {
		cfg, stale := s.cfg, s.stale
		s.mu.RUnlock()
		return cfg, stale
	}
	s.mu.RUnlock()

	s.reload()

	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg, s.stale
}

// Name returns the active config's name (empty when none).
func (s *CutConfigStore) Name() string {
	cfg, _ := s.Current()
	if cfg == nil {
		return ""
	}
	return cfg.Name
}

// LastError returns the most recent load error, if any.
func (s *CutConfigStore) LastError() string {
	if s == nil {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastErr
}

func (s *CutConfigStore) reload() {
	token := s.src.Token()
	cfg, err := s.src.Load()

	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		// Keep the last good parse (if any) and flag stale. The token is left
		// unchanged so a later good Token()==token short-circuit can't mask the
		// failure; we retry on the next Current() until the source recovers.
		s.stale = s.cfg != nil
		s.lastErr = err.Error()
		return
	}
	s.cfg = cfg
	s.token = token
	s.loadedAt = time.Now()
	s.stale = false
	s.lastErr = ""
}

// resolveCutConfigPath is where the active .cutconfig lives: the CNC_CUTCONFIG
// env override, else a stable path in the user config dir (where a future
// upload endpoint writes it). A missing file is a neutral "no config".
func resolveCutConfigPath() string {
	if p := os.Getenv("CNC_CUTCONFIG"); p != "" {
		return p
	}
	if cfg, err := os.UserConfigDir(); err == nil && cfg != "" {
		return filepath.Join(cfg, "filebrowser-NC", "active.cutconfig")
	}
	return filepath.Join(os.TempDir(), "filebrowser-NC-active.cutconfig")
}
