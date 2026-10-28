package cnc

// The call tree of the loaded program. A 3+1 job loads a hand-written main
// that only calls other programs (M98 P2001 ...), so the setup sheet reads
// the whole tree from the share root: the main's own tools are the first
// group, each called program follows in call order, and pockets that
// disagree between programs raise alerts.
//
// Programs are found by FindProgramFile (share root only, by the O number
// inside the file). Parsed files are cached by path + mtime, so rebuilding
// the tree after a repost only re-reads what changed.

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Group states besides SheetReady and SheetNoJSON.
const SheetMissing = "missing" // a called program that is not in the share

// Alert kinds raised across programs. A machine check that is not ok for
// the group that runs next is raised under its own status (SWAP, TOUCH ...).
const (
	AlertConflict   = "CONFLICT"
	AlertTwoPockets = "TWO POCKETS"
	AlertSize       = "SIZE"
	AlertUnverified = "UNVERIFIED"
	AlertMissing    = "MISSING"
)

// maxTreeDepth caps how deep calls are followed (the main is depth 0).
const maxTreeDepth = 8

// Macro programs (G65 P9000-P9999: probing, touch-off) are not part of the
// job's tools.
const callMacroFloor, callMacroCeil = 9000, 9999

// ncInfo is what the tree reads from one .nc.
type ncInfo struct {
	Title   string   // the comment on the O line
	Calls   []string // called O numbers, file order, repeats kept
	Pockets []int    // T<n> on an M6 line, first use order, without T19/T20
}

var (
	commentRe  = regexp.MustCompile(`\([^)]*\)`)
	m98Re      = regexp.MustCompile(`(?:^|[^A-Z#.])M0*98(?:[^0-9]|$)`)
	g65Re      = regexp.MustCompile(`(?:^|[^A-Z#.])G0*65(?:[^0-9]|$)`)
	m6Re       = regexp.MustCompile(`(?:^|[^A-Z#.])M0*6(?:[^0-9]|$)`)
	pWordRe    = regexp.MustCompile(`(?:^|[^A-Z#.])P0*(\d+)`)
	tWordRe    = regexp.MustCompile(`(?:^|[^A-Z#.])T0*(\d+)`)
	oTitleRe   = regexp.MustCompile(`^\s*[Oo]0*\d+\s*\((.*)\)`)
	unclosedRe = regexp.MustCompile(`\(.*$`)
)

// parseNC reads the calls, tool pockets and title out of NC text.
func parseNC(text string) *ncInfo {
	info := &ncInfo{}
	seen := map[int]bool{}
	for i, raw := range strings.Split(text, "\n") {
		if i < 12 && info.Title == "" {
			if m := oTitleRe.FindStringSubmatch(raw); m != nil {
				info.Title = strings.TrimSpace(m[1])
			}
		}
		line := commentRe.ReplaceAllString(raw, " ")
		line = unclosedRe.ReplaceAllString(line, "")
		line = strings.ToUpper(line)
		if m98Re.MatchString(line) {
			if m := pWordRe.FindStringSubmatch(line); m != nil && m[1] != "0" {
				info.Calls = append(info.Calls, m[1])
			}
		} else if g65Re.MatchString(line) {
			if m := pWordRe.FindStringSubmatch(line); m != nil && m[1] != "0" {
				if n, _ := strconv.Atoi(m[1]); n < callMacroFloor || n > callMacroCeil {
					info.Calls = append(info.Calls, m[1])
				}
			}
		}
		if m6Re.MatchString(line) {
			if m := tWordRe.FindStringSubmatch(line); m != nil {
				n, _ := strconv.Atoi(m[1])
				if n >= 1 && n != 19 && n != 20 && !seen[n] {
					seen[n] = true
					info.Pockets = append(info.Pockets, n)
				}
			}
		}
	}
	return info
}

// fileStamp identifies a version of a file.
type fileStamp struct {
	mod  time.Time
	size int64
}

type ncEntry struct {
	stamp fileStamp
	info  *ncInfo
}

type cardsEntry struct {
	stamp fileStamp
	cards *Cards
	err   error
}

// ParseCache holds parsed .nc files and sidecars by path + mtime.
type ParseCache struct {
	mu    sync.Mutex
	nc    map[string]ncEntry
	cards map[string]cardsEntry
}

// NewParseCache is an empty cache.
func NewParseCache() *ParseCache {
	return &ParseCache{nc: map[string]ncEntry{}, cards: map[string]cardsEntry{}}
}

// defaultCache backs BuildProgramTree.
var defaultCache = NewParseCache()

func stampOf(path string) (fileStamp, error) {
	st, err := os.Stat(path)
	if err != nil {
		return fileStamp{}, err
	}
	return fileStamp{mod: st.ModTime(), size: st.Size()}, nil
}

func (c *ParseCache) ncInfo(path string) (*ncInfo, error) {
	stamp, err := stampOf(path)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	e, ok := c.nc[path]
	c.mu.Unlock()
	if ok && e.stamp == stamp {
		return e.info, nil
	}
	buf, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	info := parseNC(string(buf))
	c.mu.Lock()
	c.nc[path] = ncEntry{stamp: stamp, info: info}
	c.mu.Unlock()
	return info, nil
}

func (c *ParseCache) cardsOf(path string) (*Cards, error) {
	stamp, err := stampOf(path)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	e, ok := c.cards[path]
	c.mu.Unlock()
	if ok && e.stamp == stamp {
		return e.cards, e.err
	}
	var cards *Cards
	buf, err := os.ReadFile(path)
	if err == nil {
		cards, err = ParseCards(buf)
	}
	c.mu.Lock()
	c.cards[path] = cardsEntry{stamp: stamp, cards: cards, err: err}
	c.mu.Unlock()
	return cards, err
}

// TreeGroup is one program of the tree.
type TreeGroup struct {
	Number  string
	Title   string
	State   string // SheetReady, SheetNoJSON or SheetMissing
	Calls   int
	File    *ProgramFile // nil for a missing program
	Cards   *Cards       // nil without a (readable) sidecar
	Pockets []int        // without a sidecar: the pockets the NC changes to
}

// Tools are the sidecar tools of the group, run order, each pocket once.
func (g *TreeGroup) Tools() []CardTool {
	if g.Cards == nil {
		return nil
	}
	return g.Cards.ProgramTools()
}

// HasTools reports whether the group lists any pocket.
func (g *TreeGroup) HasTools() bool { return len(g.Tools()) > 0 || len(g.Pockets) > 0 }

// UsesPocket reports whether the group changes to the pocket.
func (g *TreeGroup) UsesPocket(p int) bool {
	for _, t := range g.Tools() {
		if t.Number == p {
			return true
		}
	}
	if g.Cards == nil {
		for _, q := range g.Pockets {
			if q == p {
				return true
			}
		}
	}
	return false
}

// ProgramTree is the loaded program and everything it calls.
type ProgramTree struct {
	Dir    string
	Groups []*TreeGroup // [0] is the loaded program
	// Alerts are the ones the files alone decide (see CrossAlerts).
	Alerts []SetupAlert

	stamps map[string]fileStamp // every member file, as read
	absent []string             // sidecars looked for and not found
}

// Main is the loaded program's group.
func (t *ProgramTree) Main() *TreeGroup { return t.Groups[0] }

// Index is the group of the program number, -1 when it is not in the tree.
func (t *ProgramTree) Index(number string) int {
	if t == nil {
		return -1
	}
	for i, g := range t.Groups {
		if g.Number == number {
			return i
		}
	}
	return -1
}

// QRProgram is the program whose setup sheet the QR opens: the loaded
// program when it has one, else the first group in call order that does.
// "" when none does.
func (t *ProgramTree) QRProgram() string {
	for _, g := range t.Groups {
		if g.File != nil && g.File.Doc != "" {
			return g.Number
		}
	}
	return ""
}

// ConflictPocket reports whether pocket carries a CONFLICT alert.
func (t *ProgramTree) ConflictPocket(p int) bool {
	for _, a := range t.Alerts {
		if a.Kind == AlertConflict && a.Pocket == p {
			return true
		}
	}
	return false
}

// TreeTool is a sidecar tool and the group that lists it.
type TreeTool struct {
	Group int
	Index int // position in the group's Tools()
	CardTool
}

// UniqueTools are the sidecar tools of every group, deduped by pocket plus
// identity, in group then run order.
func (t *ProgramTree) UniqueTools() []TreeTool {
	var out []TreeTool
	for gi, g := range t.Groups {
		for ci, ct := range g.Tools() {
			dup := false
			for _, o := range out {
				if o.Number == ct.Number && IdentityOf(o.CardTool).Same(IdentityOf(ct)) {
					dup = true
					break
				}
			}
			if !dup {
				out = append(out, TreeTool{Group: gi, Index: ci, CardTool: ct})
			}
		}
	}
	return out
}

// Stale reports whether a member file changed since the tree was read: a
// .nc or sidecar with a new mtime, a sidecar that appeared, or a program
// that was missing or not yet found.
func (t *ProgramTree) Stale() bool {
	for _, g := range t.Groups {
		if g.File == nil {
			return true
		}
	}
	for path, was := range t.stamps {
		if now, err := stampOf(path); err != nil || now != was {
			return true
		}
	}
	for _, path := range t.absent {
		if _, err := os.Stat(path); err == nil {
			return true
		}
	}
	return false
}

// BuildProgramTree reads number and everything it calls from dir. It never
// returns nil for a program number; one that is not on disk is a lone
// "nojson" group.
func BuildProgramTree(dir, number string) *ProgramTree {
	return BuildProgramTreeCached(defaultCache, dir, number)
}

// BuildProgramTreeCached is BuildProgramTree with an explicit cache.
func BuildProgramTreeCached(c *ParseCache, dir, number string) *ProgramTree {
	t := &ProgramTree{Dir: dir, stamps: map[string]fileStamp{}}
	index := map[string]*TreeGroup{}

	var visit func(num string, depth int, stack map[string]bool) *TreeGroup
	visit = func(num string, depth int, stack map[string]bool) *TreeGroup {
		g := &TreeGroup{Number: num, Calls: 1}
		index[num] = g
		t.Groups = append(t.Groups, g)
		pf, err := FindProgramFile(dir, num)
		if err != nil || pf == nil {
			g.State = SheetMissing
			if depth == 0 {
				g.State = SheetNoJSON // the loaded program is not in the share
			}
			return g
		}
		g.File = pf
		var info *ncInfo
		if i, err := c.ncInfo(pf.NC); err == nil {
			info = i
			if st, err := stampOf(pf.NC); err == nil {
				t.stamps[pf.NC] = st
			}
		} else {
			info = &ncInfo{}
		}
		g.Title = asciiOnly(info.Title)
		g.State = SheetNoJSON
		if pf.Sidecar != "" {
			if st, err := stampOf(pf.Sidecar); err == nil {
				t.stamps[pf.Sidecar] = st
			}
			if cards, err := c.cardsOf(pf.Sidecar); err == nil {
				g.Cards, g.State = cards, SheetReady
				if cc := asciiOnly(cards.Comment); cc != "" {
					g.Title = cc
				}
			}
		} else {
			t.absent = append(t.absent, strings.TrimSuffix(pf.NC, ".nc")+".json")
		}
		if g.Cards == nil {
			g.Pockets = info.Pockets
		}
		stack[num] = true
		for _, callee := range info.Calls {
			if cg := index[callee]; cg != nil {
				cg.Calls++ // a second call, or a cycle back up the stack
				continue
			}
			if depth+1 > maxTreeDepth {
				continue
			}
			visit(callee, depth+1, stack)
		}
		delete(stack, num)
		return g
	}
	visit(number, 0, map[string]bool{})
	t.Alerts = crossAlerts(t.Groups)
	return t
}

func progName(g *TreeGroup) string { return "O" + g.Number }

// use is one group's claim on a pocket.
type use struct {
	g    *TreeGroup
	tool *CardTool // nil: from the NC, identity unknown
}

func absf(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

// crossAlerts compares every pocket used by more than one group.
func crossAlerts(groups []*TreeGroup) []SetupAlert {
	var out []SetupAlert
	for _, g := range groups {
		if g.State == SheetMissing {
			out = append(out, SetupAlert{Kind: AlertMissing, Programs: []string{progName(g)},
				Detail: progName(g) + " is called but is not in the share"})
		}
	}

	byPocket := map[int][]use{}
	for _, g := range groups {
		if g.Cards != nil {
			tools := g.Tools()
			for i := range tools {
				byPocket[tools[i].Number] = append(byPocket[tools[i].Number], use{g: g, tool: &tools[i]})
			}
			continue
		}
		for _, p := range g.Pockets {
			byPocket[p] = append(byPocket[p], use{g: g})
		}
	}
	raised := map[string]bool{}
	pockets := make([]int, 0, len(byPocket))
	for p := range byPocket {
		pockets = append(pockets, p)
	}
	sort.Ints(pockets)

	for _, p := range pockets {
		uses := byPocket[p]
		if len(uses) < 2 {
			continue
		}
		var reps []use // distinct sidecar tools seen so far
		var nojson, sidecar []string
		for _, u := range uses {
			if u.tool == nil {
				nojson = append(nojson, progName(u.g))
				continue
			}
			sidecar = append(sidecar, progName(u.g))
			matched := false
			for _, r := range reps {
				a := alertFor(p, r, u)
				if a == nil {
					matched = true
					continue
				}
				key := a.Kind + "|" + strings.Join(a.Programs, "|")
				if !raised[key+"|"+strconv.Itoa(p)] {
					raised[key+"|"+strconv.Itoa(p)] = true
					out = append(out, *a)
				}
			}
			if !matched {
				reps = append(reps, u)
			}
		}
		if len(nojson) > 0 && len(sidecar) > 0 && !IsFixedPocket(p) {
			out = append(out, SetupAlert{Kind: AlertUnverified, Pocket: p,
				Programs: append(append([]string{}, nojson...), sidecar...),
				Detail:   fmt.Sprintf("T%d is shared by %s (no sidecar, not size-checked) and %s", p, strings.Join(nojson, ", "), strings.Join(sidecar, ", "))})
		}
	}

	// The same tool under different pockets in different programs.
	type seenAt struct {
		g    *TreeGroup
		pock int
		tool CardTool
	}
	var all []seenAt
	for _, g := range groups {
		for _, t := range g.Tools() {
			all = append(all, seenAt{g, t.Number, t})
		}
	}
	done := map[[2]int]bool{}
	for i, a := range all {
		for _, b := range all[i+1:] {
			if a.g == b.g || a.pock == b.pock || !IdentityOf(a.tool).Same(IdentityOf(b.tool)) {
				continue
			}
			lo, hi := a.pock, b.pock
			if lo > hi {
				lo, hi = hi, lo
			}
			if done[[2]int{lo, hi}] {
				continue
			}
			done[[2]int{lo, hi}] = true
			out = append(out, SetupAlert{Kind: AlertTwoPockets, Pocket: lo,
				Programs: []string{progName(a.g), progName(b.g)},
				Detail:   fmt.Sprintf("%s is T%d in %s and T%d in %s", ToolLabel(a.tool), a.pock, progName(a.g), b.pock, progName(b.g))})
		}
	}
	return out
}

// alertFor compares two sidecar tools in one pocket; nil when they are the
// same tool or close enough to share it.
func alertFor(p int, a, b use) *SetupAlert {
	ta, tb := *a.tool, *b.tool
	progs := []string{progName(a.g), progName(b.g)}
	if a.g == b.g {
		return nil
	}
	if !strings.EqualFold(strings.TrimSpace(ta.Type), strings.TrimSpace(tb.Type)) {
		return &SetupAlert{Kind: AlertConflict, Pocket: p, Programs: progs,
			Detail: fmt.Sprintf("T%d swap between %s and %s (%s, %s)", p, progs[0], progs[1], ToolLabel(ta), ToolLabel(tb))}
	}
	d := absf(ta.Diameter - tb.Diameter)
	if d <= identityDiaTol {
		return nil
	}
	if !UsesDiameter(ta) {
		return &SetupAlert{Kind: AlertConflict, Pocket: p, Programs: progs,
			Detail: fmt.Sprintf("T%d swap between %s and %s (%s, %s)", p, progs[0], progs[1], ToolLabel(ta), ToolLabel(tb))}
	}
	tol := DiameterTol
	if isFace(ta) {
		tol = FaceTolFraction * ta.Diameter
	}
	if d > tol {
		return &SetupAlert{Kind: AlertSize, Pocket: p, Programs: progs,
			Detail: fmt.Sprintf("T%d dia %.4f in %s, %.4f in %s", p, ta.Diameter, progs[0], tb.Diameter, progs[1])}
	}
	return nil
}
