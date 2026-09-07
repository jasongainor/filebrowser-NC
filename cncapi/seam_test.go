package cncapi

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRootResolver_JoinsInsideRoot(t *testing.T) {
	root := t.TempDir()
	rr := NewRootResolver(root)

	cases := []struct {
		rel  string
		want string
	}{
		{"", root},
		{"/", root},
		{"foo.nc", filepath.Join(root, "foo.nc")},
		{"/foo.nc", filepath.Join(root, "foo.nc")},
		{"a/b/c.nc", filepath.Join(root, "a/b/c.nc")},
		{"/a/b/c.nc", filepath.Join(root, "a/b/c.nc")},
	}
	for _, tc := range cases {
		got, err := rr.FullPath(tc.rel)
		if err != nil {
			t.Fatalf("FullPath(%q): unexpected error: %v", tc.rel, err)
		}
		if got != tc.want {
			t.Errorf("FullPath(%q) = %q, want %q", tc.rel, got, tc.want)
		}
	}
}

func TestRootResolver_RejectsEscapes(t *testing.T) {
	root := t.TempDir()
	rr := NewRootResolver(root)

	escapes := []string{
		"..",
		"../",
		"../etc/passwd",
		"../../etc/passwd",
		"a/../../b",
		"a/../../../b",
		strings.Repeat("../", 20) + "etc/passwd",
	}
	for _, rel := range escapes {
		got, err := rr.FullPath(rel)
		if err == nil {
			t.Errorf("FullPath(%q) = %q, <nil error>; want an error", rel, got)
		}
	}
}

func TestRootResolver_DeepRootStillRejectsEscape(t *testing.T) {
	// A root with several path segments makes it easier to
	// accidentally "succeed" at escaping one level while still
	// landing under a sibling that happens to share a prefix
	// (e.g. root=/srv/data escaping to /srv/data-backup). Guard
	// against that class of bug explicitly.
	root := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	rr := NewRootResolver(root)

	if _, err := rr.FullPath("../data-backup/secret"); err == nil {
		t.Errorf("expected escape into a sibling directory to be rejected")
	}
}
