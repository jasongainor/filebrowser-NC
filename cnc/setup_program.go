package cnc

// Finding the program file for the program the control has loaded. The
// match is on the O number written INSIDE the file, never on the file
// name: "30011.nc" holds O03001 and both "3101.nc" and
// "3101 - 4B326FE OP0a.nc" hold O03101. When several files carry the
// same O number the newest one is the one Jason just posted.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var (
	oLineRe  = regexp.MustCompile(`^\s*[Oo]0*(\d+)`)
	loadedRe = regexp.MustCompile(`^\s*[Oo]?0*(\d+)\b`)
)

// LoadedProgramNumber normalizes the program token Q500 reports ("O00057",
// "O31011") to its bare number ("57", "31011"). "" when no numbered program
// is loaded (MDI, empty, anything else).
func LoadedProgramNumber(token string) string {
	m := loadedRe.FindStringSubmatch(token)
	if m == nil || m[1] == "0" {
		return ""
	}
	return m[1]
}

// FileProgramNumber returns the O number of NC text: the first O-word line
// in the opening lines (a leading "%" is skipped). "" when there is none.
func FileProgramNumber(text string) string {
	for i, line := range strings.Split(text, "\n") {
		if i >= 12 {
			break
		}
		if m := oLineRe.FindStringSubmatch(line); m != nil && m[1] != "0" {
			return m[1]
		}
	}
	return ""
}

// ProgramFile is the NC file chosen for a loaded program.
type ProgramFile struct {
	NC      string    // absolute path of the .nc
	Sidecar string    // absolute path of <base>.json, "" when it does not exist
	ModTime time.Time // of the .nc
}

// FindProgramFile scans dir (not recursively) for .nc files whose O line is
// number, and returns the newest, or nil when there is none.
func FindProgramFile(dir, number string) (*ProgramFile, error) {
	if number == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var best *ProgramFile
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.EqualFold(filepath.Ext(name), ".nc") {
			continue
		}
		full := filepath.Join(dir, name)
		info, err := e.Info()
		if err != nil {
			continue
		}
		if best != nil && !info.ModTime().After(best.ModTime) {
			continue
		}
		head, err := readHead(full, 2048)
		if err != nil || FileProgramNumber(head) != number {
			continue
		}
		best = &ProgramFile{NC: full, ModTime: info.ModTime()}
	}
	if best == nil {
		return nil, nil
	}
	side := strings.TrimSuffix(best.NC, filepath.Ext(best.NC)) + ".json"
	if st, err := os.Stat(side); err == nil && !st.IsDir() {
		best.Sidecar = side
	}
	return best, nil
}

func readHead(path string, n int) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	buf := make([]byte, n)
	k, _ := f.Read(buf)
	return string(buf[:k]), nil
}
