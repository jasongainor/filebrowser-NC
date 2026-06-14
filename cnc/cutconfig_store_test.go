package cnc

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// fakeSource is a controllable CutConfigSource that counts Load() calls so we
// can prove the store parses once and only re-parses when the token changes.
type fakeSource struct {
	token string
	loads int
	cfg   *CutConfig
	err   error
}

func (f *fakeSource) Token() string             { return f.token }
func (f *fakeSource) Load() (*CutConfig, error) { f.loads++; return f.cfg, f.err }

func TestCutConfigStoreParsesOnce(t *testing.T) {
	fs := &fakeSource{token: "v1", cfg: &CutConfig{Name: "alu"}}
	st := NewCutConfigStore(fs) // initial load

	for i := 0; i < 5; i++ {
		cfg, stale := st.Current()
		if cfg == nil || cfg.Name != "alu" || stale {
			t.Fatalf("Current()#%d = %v stale=%v", i, cfg, stale)
		}
	}
	if fs.loads != 1 {
		t.Errorf("Load() called %d times, want 1 (token unchanged → no re-parse)", fs.loads)
	}
}

func TestCutConfigStoreReloadsOnTokenChange(t *testing.T) {
	fs := &fakeSource{token: "v1", cfg: &CutConfig{Name: "alu"}}
	st := NewCutConfigStore(fs)
	st.Current()

	fs.token = "v2"
	fs.cfg = &CutConfig{Name: "steel"}
	cfg, _ := st.Current()
	if cfg.Name != "steel" {
		t.Errorf("after token change Name=%q, want steel", cfg.Name)
	}
	if fs.loads != 2 {
		t.Errorf("Load() called %d times, want 2", fs.loads)
	}
}

func TestCutConfigStoreServesLastGoodOnFailure(t *testing.T) {
	fs := &fakeSource{token: "v1", cfg: &CutConfig{Name: "alu"}}
	st := NewCutConfigStore(fs)
	st.Current()

	// Source breaks (e.g. corrupt zip / API down) with a changed token.
	fs.token = "v2"
	fs.err = errors.New("boom")
	cfg, stale := st.Current()
	if cfg == nil || cfg.Name != "alu" {
		t.Errorf("want last-good config 'alu', got %v", cfg)
	}
	if !stale {
		t.Errorf("want stale=true after reload failure")
	}
	if st.LastError() == "" {
		t.Errorf("want LastError to surface the failure")
	}

	// Source recovers.
	fs.err = nil
	fs.cfg = &CutConfig{Name: "alu2"}
	cfg, stale = st.Current()
	if cfg.Name != "alu2" || stale {
		t.Errorf("after recovery Name=%q stale=%v, want alu2/false", cfg.Name, stale)
	}
}

func TestFileCutConfigSource(t *testing.T) {
	b := syntheticCutConfigBytes(t)
	path := filepath.Join(t.TempDir(), "test.cutconfig")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	st := NewCutConfigStore(FileCutConfigSource{Path: path})
	cfg, stale := st.Current()
	if cfg == nil || stale {
		t.Fatalf("fixture should load: cfg=%v stale=%v err=%q", cfg, stale, st.LastError())
	}
	if st.Name() != "Test Alloy" {
		t.Errorf("Name = %q, want Test Alloy", st.Name())
	}

	// Empty path = no config configured: neutral, not an error, no churn.
	empty := NewCutConfigStore(FileCutConfigSource{Path: ""})
	if cfg, stale := empty.Current(); cfg != nil || stale {
		t.Errorf("empty path should be (nil,false), got (%v,%v)", cfg, stale)
	}
	if empty.LastError() != "" {
		t.Errorf("empty path should not be an error: %q", empty.LastError())
	}

	// Missing file = neutral too (not an error).
	missing := NewCutConfigStore(FileCutConfigSource{Path: filepath.Join(t.TempDir(), "nope.cutconfig")})
	if cfg, _ := missing.Current(); cfg != nil {
		t.Errorf("missing file should yield nil config")
	}
	if missing.LastError() != "" {
		t.Errorf("missing file should not be an error: %q", missing.LastError())
	}
}
