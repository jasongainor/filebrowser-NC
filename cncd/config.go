// Package cncd is the standalone CNC daemon: the headless half of the
// seam introduced in cncapi and http/cnc_seam.go, with no filebrowser
// users, bolt DB, or Vue frontend behind it. It serves the same
// /api/cnc/* and /api/displays/{id} surface as filebrowser's fbhttp
// package, backed by a single served directory instead of a
// per-user scoped share.
//
// See docs/CNCD.md for how to run it and docs/REPO_SPLIT_TODO.md for
// why it exists.
package cncd

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sync"

	"github.com/filebrowser/filebrowser/v2/settings"
)

// Config is the on-disk shape of cncd's --config file: exactly the
// settings.Cnc document filebrowser used to keep in its bolt DB
// (machines, displays, discord, machineToken, baselinePollSeconds).
// Loading and saving this file is the entirety of cncd's persistence
// — there is no bolt DB, no users, nothing else to migrate.
type Config = settings.Cnc

// Store is a Config held in memory, backed by a JSON file on disk.
// It implements the settingsReader interface cnc.NewRegistry and its
// Streamer/Aggregator/Notifier collaborators expect (Get() (*settings.Settings,
// error)) by wrapping the loaded Cnc document in an otherwise-empty
// *settings.Settings — the cnc package never reads any field outside
// .Cnc, so the rest of that struct being zero-valued is safe.
//
// Store is safe for concurrent use. Handlers that mutate config (e.g.
// creating a display) call Update, which persists to disk before
// returning so a crash right after never loses an acknowledged write.
type Store struct {
	path string

	mu  sync.RWMutex
	cnc settings.Cnc
}

// LoadStore reads path (creating it with a zero-value Config if it
// doesn't exist yet — a fresh Pi install shouldn't need a
// hand-authored file to boot) and returns a Store backed by it.
func LoadStore(path string) (*Store, error) {
	s := &Store{path: path}
	buf, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(buf, &s.cnc); err != nil {
			return nil, fmt.Errorf("cncd: parse config %s: %w", path, err)
		}
	case os.IsNotExist(err):
		// First boot: nothing to load. Write the zero-value config
		// back immediately so the file exists for the operator to
		// find and edit, and so a later Save has something to diff
		// against on disk.
		if err := s.persist(); err != nil {
			return nil, fmt.Errorf("cncd: initialize config %s: %w", path, err)
		}
	default:
		return nil, fmt.Errorf("cncd: read config %s: %w", path, err)
	}
	// A box with no machine token would answer 401 to everything that
	// matters (state, qcode, MCP, the files API's writes). Mint one on
	// first boot the same way filebrowser's settings tab does, persist
	// it, and say where it is — never what it is.
	if s.cnc.MachineToken == "" {
		tok, err := mintMachineToken()
		if err != nil {
			return nil, fmt.Errorf("cncd: mint machine token: %w", err)
		}
		s.cnc.MachineToken = tok
		if err := s.persist(); err != nil {
			return nil, fmt.Errorf("cncd: persist minted machine token %s: %w", path, err)
		}
		log.Printf("cncd: minted a machine token into %s (machineToken)", path)
	}
	return s, nil
}

// mintMachineToken mirrors http/cnc.go's cncMachineTokenMintHandler:
// 32 random bytes, URL-safe base64, no padding.
func mintMachineToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// Get implements the settingsReader interface the cnc package expects
// (cnc.NewRegistry, cnc.New, cnc.NewNotifier all take one of these).
func (s *Store) Get() (*settings.Settings, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cp := s.cnc
	return &settings.Settings{Cnc: cp}, nil
}

// Snapshot returns a copy of the current Cnc document for handlers
// that need to read it directly (machine list, display list, machine
// token) without going through the settings.Settings wrapper.
func (s *Store) Snapshot() settings.Cnc {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cnc
}

// Update applies fn to the current config under the write lock and
// persists the result to disk before returning. fn mutates cnc in
// place. Returns whatever error Update or fn produced; on error the
// in-memory config is left as fn last modified it (fn should not
// perform partial mutations it can't tolerate being visible on a
// later successful Update — none of cncd's callers do).
func (s *Store) Update(fn func(cnc *settings.Cnc) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Mutate a copy so a failing fn cannot leave half-applied state in
	// memory; only a successful edit becomes the live config.
	next := s.cnc
	if err := fn(&next); err != nil {
		return err
	}
	prev := s.cnc
	s.cnc = next
	if err := s.persist(); err != nil {
		s.cnc = prev
		return err
	}
	return nil
}

// persist writes the current config to disk. Caller must hold s.mu
// (read or write — persist only reads s.cnc).
func (s *Store) persist() error {
	buf, err := json.MarshalIndent(s.cnc, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
