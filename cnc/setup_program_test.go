package cnc

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadedProgramNumber(t *testing.T) {
	for in, want := range map[string]string{
		"O31011": "31011", "O00057": "57", "O03101": "3101", "3101": "3101",
		"o03001": "3001", "O31011 (4B326FE)": "31011", "MDI": "", "": "", "O00000": "",
	} {
		if got := LoadedProgramNumber(in); got != want {
			t.Errorf("LoadedProgramNumber(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFileProgramNumber(t *testing.T) {
	if got := FileProgramNumber("%\nO03001 (4B326FD)\nN10 G90\n"); got != "3001" {
		t.Fatalf("got %q", got)
	}
	if got := FileProgramNumber("N10 G90\nM30\n"); got != "" {
		t.Fatalf("got %q", got)
	}
}

func put(t *testing.T, dir, name, body string, age time.Duration) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(p, when, when); err != nil {
		t.Fatal(err)
	}
}

func TestFindProgramFile(t *testing.T) {
	dir := t.TempDir()
	// File name and O number disagree: 30011.nc holds O03001.
	put(t, dir, "30011.nc", "%\nO03001 (x)\nM30\n", time.Hour)
	// Two files for O03101, newest wins; only the older one is "3101.nc".
	put(t, dir, "3101.nc", "%\nO03101 (old)\nM30\n", 2*time.Hour)
	put(t, dir, "3101 - 4B326FE OP0a.nc", "%\nO03101 (new)\nM30\n", time.Minute)
	put(t, dir, "3101 - 4B326FE OP0a.json", "{}", time.Minute)
	// A file named for the number that holds a different O number.
	put(t, dir, "3001.nc", "%\nO09999\nM30\n", time.Minute)
	put(t, dir, "notes.txt", "O03101", 0)

	pf, err := FindProgramFile(dir, "3001")
	if err != nil || pf == nil || filepath.Base(pf.NC) != "30011.nc" {
		t.Fatalf("3001 -> %+v err=%v (must match the O line, not the file name)", pf, err)
	}
	if pf.Sidecar != "" {
		t.Fatalf("30011.nc has no sidecar, got %q", pf.Sidecar)
	}

	pf, _ = FindProgramFile(dir, "3101")
	if pf == nil || filepath.Base(pf.NC) != "3101 - 4B326FE OP0a.nc" {
		t.Fatalf("3101 -> %+v (newest must win)", pf)
	}
	if filepath.Base(pf.Sidecar) != "3101 - 4B326FE OP0a.json" {
		t.Fatalf("sidecar -> %q", pf.Sidecar)
	}

	if pf, _ = FindProgramFile(dir, "424242"); pf != nil {
		t.Fatalf("unknown program matched %+v", pf)
	}
	// Re-posting makes the old name the newest again.
	put(t, dir, "3101.nc", "%\nO03101 (reposted)\nM30\n", 0)
	pf, _ = FindProgramFile(dir, "3101")
	if filepath.Base(pf.NC) != "3101.nc" {
		t.Fatalf("after repost got %q", pf.NC)
	}
}

func TestFindProgramFileFolder(t *testing.T) {
	dir := t.TempDir()
	put(t, dir, "3202.nc", "%\nO03202 (x)\nM30\n", time.Minute)
	if err := os.Mkdir(filepath.Join(dir, "3202"), 0o755); err != nil {
		t.Fatal(err)
	}
	pf, _ := FindProgramFile(dir, "3202")
	if pf == nil || pf.Folder == "" || pf.Sidecar != "" || pf.Doc != "" {
		t.Fatalf("empty folder -> %+v", pf)
	}

	// The sidecar moved into the folder, with a setup sheet beside it.
	put(t, dir, "3202/3202.json", "{}", time.Minute)
	put(t, dir, "3202/old sheet.html", "<p>", time.Hour)
	put(t, dir, "3202/3202.html", "<p>", time.Minute)
	put(t, dir, "3202/.3202.html", "<p>", 0)
	pf, _ = FindProgramFile(dir, "3202")
	if pf.Sidecar != filepath.Join(dir, "3202", "3202.json") {
		t.Fatalf("sidecar -> %q", pf.Sidecar)
	}
	if pf.Doc != "3202.html" {
		t.Fatalf("doc -> %q (newest html, no dot-files)", pf.Doc)
	}

	// A sidecar next to the .nc still wins.
	put(t, dir, "3202.json", "{}", time.Minute)
	pf, _ = FindProgramFile(dir, "3202")
	if pf.Sidecar != filepath.Join(dir, "3202.json") {
		t.Fatalf("sidecar -> %q (next to the .nc wins)", pf.Sidecar)
	}

	// Any json in the folder when it is not named for the program.
	if err := os.Remove(filepath.Join(dir, "3202.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, "3202", "3202.json"), filepath.Join(dir, "3202", "tools.json")); err != nil {
		t.Fatal(err)
	}
	pf, _ = FindProgramFile(dir, "3202")
	if filepath.Base(pf.Sidecar) != "tools.json" {
		t.Fatalf("sidecar -> %q", pf.Sidecar)
	}
}
