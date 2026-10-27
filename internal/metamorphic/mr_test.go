package metamorphic

// The metamorphic relations on the fixtures (testdata/dataflow's flow and
// sdg packages, and chi), run by go test ./... The same harness runs them on
// the prepared eval apps behind the `metamorphic` build tag
// (realapp_test.go, scripts/metamorphic.sh).
//
// Each relation transforms a copy of the module, analyzes it, and checks
// one of three oracles against the original's analysis:
//
//   - exact: the analysis is unchanged up to the position map (MR1-MR4,
//     MR7, MR15);
//   - delta: it changes by exactly the delta each site declares (MR8-MR12),
//     or by at most a declared removal (MR13, a precision relation);
//   - reach: every flow of the original survives (MR5, MR6, MR14).
//
// Every relation logs one MRSTAT line (sites, skips, pairs checked), which
// scripts/metamorphic.sh collects.

import (
	"fmt"
	"go/ast"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// seed is the fixed seed of every sample and site choice.
const seed = 1

// target is one module the relations run on.
type target struct {
	name string
	dir  string
	// pkgDirs restricts the transformed files to these package directories
	// (all when empty).
	pkgDirs []string
	// nFiles and nCallables bound the samples (all when 0).
	nFiles, nCallables int

	once sync.Once
	ws   *Workspace
	pre  *analyzed // -a 4
	pre3 *analyzed // -a 3
	err  error
	mu   sync.Mutex
}

// analyzed is one analyzed module.
type analyzed struct {
	view    *View
	errPkgs map[string]bool // packages with load errors
	dur     time.Duration
}

func analyze(dir string, level int, targets []string) (*analyzed, error) {
	start := time.Now()
	r, err := Analyze(dir, level, targets)
	if err != nil {
		return nil, err
	}
	a := &analyzed{view: r.View, errPkgs: map[string]bool{}, dur: time.Since(start)}
	for _, e := range r.Errors {
		a.errPkgs[e[:strings.Index(e, ": ")]] = true
	}
	return a, nil
}

func testdata(name string) string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "testdata", name)
}

var fixtures = []*target{
	{name: "dataflow", dir: testdata("dataflow"), pkgDirs: []string{"flow", "sdg"}},
	{name: "chi", dir: testdata("chi"), nFiles: 12, nCallables: 60},
}

// load loads the target's workspace and its original analysis once.
func (tg *target) load(t *testing.T) {
	t.Helper()
	tg.once.Do(func() {
		tg.ws, tg.err = LoadWorkspace(tg.dir)
		if tg.err == nil {
			tg.pre, tg.err = analyze(tg.ws.Dir, 4, nil)
		}
	})
	if tg.err != nil {
		t.Fatalf("%s: %v", tg.name, tg.err)
	}
}

func (tg *target) level3(t *testing.T) *analyzed {
	tg.mu.Lock()
	defer tg.mu.Unlock()
	if tg.pre3 == nil {
		a, err := analyze(tg.ws.Dir, 3, nil)
		if err != nil {
			t.Fatal(err)
		}
		tg.pre3 = a
	}
	return tg.pre3
}

// files is the sample of eligible files the relations transform.
func (tg *target) files() []string {
	return Sample(tg.ws.Eligible(tg.pkgDirs...), tg.nFiles, seed)
}

// relation is one metamorphic relation.
type relation struct {
	name   string
	t      Transform
	oracle string // exact | delta | reach
	fix    Fix    // spelling fix for the transformed view (MR4)
}

// outcome is what one relation found on one target.
type outcome struct {
	sites      int
	skipped    map[string]int
	violations []string
	pairs      int
	summaries  int // MR14a summary edges checked
	dur        time.Duration
}

func (o *outcome) fail(format string, a ...any) {
	o.violations = append(o.violations, fmt.Sprintf(format, a...))
}

// logStat writes the relation's MRSTAT line.
func logStat(t *testing.T, mr, tg string, o *outcome) {
	t.Helper()
	var sk []string
	for _, k := range slices.Sorted(maps.Keys(o.skipped)) {
		sk = append(sk, fmt.Sprintf("%s=%d", k, o.skipped[k]))
	}
	t.Logf("MRSTAT %s %s sites=%d pairs=%d summaries=%d violations=%d time=%s skipped=[%s]",
		mr, tg, o.sites, o.pairs, o.summaries, len(o.violations), o.dur.Round(time.Millisecond), strings.Join(sk, "; "))
}

// transformAndAnalyze applies r to tg and analyzes the result at -a 4. A
// transformed module that type-checks worse than the original is a test
// bug and fails the test.
func transformAndAnalyze(t *testing.T, tg *target, tr Transform, ws *Workspace, files []string, pre *View) (*Result, *analyzed) {
	t.Helper()
	res, err := ws.Apply(tr, files, filepath.Join(t.TempDir(), "m"), seed, pre)
	if err != nil {
		t.Fatalf("%s on %s: %v", tr.Name, tg.name, err)
	}
	if len(res.Files) == 0 {
		return res, nil
	}
	post, err := analyze(res.Dir, 4, nil)
	if err != nil {
		t.Fatalf("%s on %s: analyze: %v", tr.Name, tg.name, err)
	}
	checkTypes(t, tg, tr.Name, res, post)
	return res, post
}

// checkTypes fails when the transformed module has a package with load
// errors the original had none in.
func checkTypes(t *testing.T, tg *target, mr string, res *Result, post *analyzed) {
	t.Helper()
	for p := range post.errPkgs {
		if !tg.pre.errPkgs[p] {
			r, _ := Analyze(res.Dir, 1, nil)
			var msgs []string
			for _, e := range r.Errors {
				if strings.HasPrefix(e, p+": ") {
					msgs = append(msgs, e)
				}
			}
			t.Fatalf("%s on %s: the transformed module does not type-check (a transform bug): %s (kept at %s)",
				mr, tg.name, strings.Join(msgs, "; "), keep(t, res.Dir))
		}
	}
}

// keep copies a failing transformed module out of the test's temp dir.
func keep(t *testing.T, dir string) string {
	dst, err := os.MkdirTemp("", "mr-fail-*")
	if err != nil {
		return dir
	}
	if err := copyTree(dir, dst); err != nil {
		return dir
	}
	return dst
}

// runRelation runs r on tg and reports its violations.
func runRelation(t *testing.T, tg *target, r relation) *outcome {
	t.Helper()
	tg.load(t)
	start := time.Now()
	o := &outcome{skipped: map[string]int{}}
	defer func() { o.dur = time.Since(start) }()
	pre := tg.pre.view
	res, post := transformAndAnalyze(t, tg, r.t, tg.ws, tg.files(), pre)
	maps.Copy(o.skipped, res.Skipped)
	o.sites = len(res.Sites)
	if post == nil {
		return o
	}
	got := post.view
	if r.fix != nil {
		got = got.Apply(r.fix)
	}
	mapped, unmapped := pre.Map(res.Map)
	if r.oracle != "reach" && len(unmapped) > 0 {
		o.fail("keys of the original do not map: %s", strings.Join(head(unmapped, 10), ", "))
		return o
	}
	switch r.oracle {
	case "exact":
		for _, d := range Diff(mapped, got, Scope{}, 40) {
			o.fail("%s", d)
		}
	case "delta":
		checkDelta(o, pre, mapped, got, res)
	case "reach":
		checkReach(o, tg, pre, got, res)
	}
	if len(o.violations) > 0 {
		t.Logf("%s on %s: transformed module kept at %s", r.name, tg.name, keep(t, res.Dir))
	}
	return o
}

func head(s []string, n int) []string {
	if len(s) > n {
		return append(slices.Clone(s[:n]), fmt.Sprintf("… and %d more", len(s)-n))
	}
	return s
}

// checkDelta is the delta oracle: the transformed view equals the original
// with every site's delta applied (MR8-MR12), and for a precision site
// (MR13) the site's checks hold.
func checkDelta(o *outcome, pre, mapped, got *View, res *Result) {
	var ds []Delta
	atMost := map[string]bool{}
	for _, s := range res.Sites {
		if s.Delta != nil {
			ds = append(ds, *s.Delta)
			if s.Delta.AtMost {
				atMost[s.Delta.Callable] = true
			}
		}
	}
	want, err := Expect(pre, mapped, res.Map, res.Marks, ds)
	if err != nil {
		o.fail("relation: %v", err)
		return
	}
	if len(atMost) == 0 {
		for _, d := range Diff(want, got, Scope{}, 40) {
			o.fail("%s", d)
		}
		return
	}
	// A precision relation: nothing added anywhere; outside the sites only
	// summary edges may go; at a site, its declared removals and summary
	// edges.
	for _, d := range Diff(want, got, Scope{Skip: atMost}, 0) {
		if strings.Contains(d, "|") && strings.HasSuffix(d, " missing") && strings.Contains(d, ": edge sum|") {
			continue
		}
		o.fail("%s", d)
	}
	for _, d := range CheckAtMost(pre, mapped, got, res.Map, res.Marks, ds) {
		if strings.Contains(d, ": sum|") && strings.HasSuffix(d, " removed") {
			continue
		}
		o.fail("%s", d)
	}
	g := got.Graph(true)
	l := newLocator(pre, got, res.Map)
	for _, s := range res.Sites {
		if len(s.NoFlow) == 0 {
			continue
		}
		c := got.Calls[s.Callable]
		var srcs []string
		for _, k := range slices.Sorted(maps.Keys(c.Body)) {
			if strings.HasPrefix(k, "@formal_in:") && !strings.HasPrefix(strings.Split(c.Body[k], "|")[2], "<global>:") {
				srcs = append(srcs, Node(s.Callable, k))
			}
		}
		for _, ref := range s.NoFlow {
			dst, ok := l.locate(Node(s.Callable, ref.Pre))
			if !ok {
				o.fail("%s: no counterpart of %s", s.Desc, ref.Pre)
				continue
			}
			if g.ReachesFrom(srcs, dst) {
				o.fail("%s: a formal_in still reaches %s (a constant argument cannot carry a flow)", s.Desc, dst)
			}
		}
	}
}

// checkReach is the reachability oracle, plus MR14's summary checks.
func checkReach(o *outcome, tg *target, pre, got *View, res *Result) {
	var cut []string
	for _, s := range res.Sites {
		cut = append(cut, s.Cut...)
	}
	rr := CheckReach(pre, got, res.Map, res.Touched, tg.nCallables, seed, cut...)
	o.pairs = rr.Pairs
	gPre := pre.Graph(false)
	for i, l := range head(rr.Lost, 40) {
		msg := "lost flow " + l
		if p := strings.SplitN(l, " -> ", 2); i < 5 && len(p) == 2 {
			msg += "\n\t\twitness in the original: " + strings.Join(gPre.Witness(p[0], p[1]), "\n\t\t  ")
		}
		o.fail("%s", msg)
	}
	for _, u := range head(rr.Unlocated, 20) {
		o.fail("no counterpart: %s", u)
	}
	if rr.NLost > len(rr.Lost) || rr.NUnlocated > len(rr.Unlocated) {
		o.fail("%d lost flows and %d unlocated nodes in all", rr.NLost, rr.NUnlocated)
	}
	l := newLocator(pre, got, res.Map)
	for _, s := range res.Sites {
		for _, sc := range s.Summary {
			o.summaries++
			src, ok1 := l.locate(Node(s.Callable, sc.Src.Pre))
			dst, ok2 := l.locate(Node(s.Callable, sc.Dst.Pre))
			if !ok1 || !ok2 {
				o.fail("%s: summary %s: no counterpart", s.Desc, sc.Pre)
				continue
			}
			id, sk := splitNode(src)
			_, dk := splitNode(dst)
			if c := got.Calls[id]; c == nil || !c.Edges["sum|"+sk+"|"+dk+"|"] {
				o.fail("%s: summary %s has no counterpart %s -> %s", s.Desc, sc.Pre, sk, dk)
			}
		}
	}
}

// ─── the relations ────────────────────────────────────────────────────────────

var relations = []relation{
	{name: "MR1", t: MR1, oracle: "exact"},
	{name: "MR2", t: MR2, oracle: "exact"},
	{name: "MR3", t: MR3, oracle: "exact"},
	{name: "MR4", t: MR4, oracle: "exact", fix: StripSuffix(Suffix4)},
	{name: "MR5", t: MR5, oracle: "reach"},
	{name: "MR6", t: MR6, oracle: "reach"},
	{name: "MR8", t: MR8, oracle: "delta"},
	{name: "MR9", t: MR9, oracle: "delta"},
	{name: "MR10", t: MR10, oracle: "delta"},
	{name: "MR11", t: MR11, oracle: "delta"},
	{name: "MR12", t: MR12, oracle: "delta"},
	{name: "MR13", t: MR13, oracle: "delta"},
	{name: "MR14", t: MR14, oracle: "reach"},
}

// knownSkips names the relation/target pairs skipped for a filed issue.
var knownSkips = map[string]string{}

func runOn(t *testing.T, targets []*target, r relation, requireSites bool) {
	sites, ran := 0, 0
	var mu sync.Mutex
	t.Run(r.name, func(t *testing.T) {
		for _, tg := range targets {
			t.Run(tg.name, func(t *testing.T) {
				if why, ok := knownSkips[r.name+"/"+tg.name]; ok {
					t.Skip(why)
				}
				t.Parallel()
				o := runRelation(t, tg, r)
				mu.Lock()
				sites += o.sites
				ran++
				mu.Unlock()
				logStat(t, r.name, tg.name, o)
				for _, v := range o.violations {
					t.Errorf("%s on %s: %s", r.name, tg.name, v)
				}
			})
		}
	})
	if requireSites && ran == len(targets) && sites == 0 {
		t.Errorf("%s exercised no site on the fixtures", r.name)
	}
}

func TestMR_Fixtures(t *testing.T) {
	for _, r := range relations {
		runOn(t, fixtures, r, true)
	}
	t.Run("MR7", func(t *testing.T) {
		for _, tg := range fixtures {
			t.Run(tg.name, func(t *testing.T) {
				t.Parallel()
				o := runMR7(t, tg)
				logStat(t, "MR7", tg.name, o)
				for _, v := range o.violations {
					t.Errorf("MR7 on %s: %s", tg.name, v)
				}
			})
		}
	})
	t.Run("MR15", func(t *testing.T) {
		for _, tg := range fixtures {
			t.Run(tg.name, func(t *testing.T) {
				t.Parallel()
				o := runMR15(t, tg)
				logStat(t, "MR15", tg.name, o)
				for _, v := range o.violations {
					t.Errorf("MR15 on %s: %s", tg.name, v)
				}
			})
		}
	})
}

// runMR7 is MR7: --target-files at -a 3 on one sampled file equals the
// full -a 3 run on that file's callables.
func runMR7(t *testing.T, tg *target) *outcome {
	tg.load(t)
	start := time.Now()
	o := &outcome{skipped: map[string]int{}}
	defer func() { o.dur = time.Since(start) }()
	full := tg.level3(t)
	for _, rel := range Sample(tg.files(), 2, seed) {
		part, err := analyze(tg.ws.Dir, 3, []string{filepath.Join(tg.ws.Dir, rel)})
		if err != nil {
			t.Fatal(err)
		}
		only := map[string]bool{}
		for id, c := range full.view.Calls {
			if c.File == rel {
				only[id] = true
			}
		}
		o.sites++
		for _, d := range Diff(full.view, part.view, Scope{Only: only}, 40) {
			o.fail("%s: %s", rel, d)
		}
	}
	return o
}

// runMR15 is MR15: MR6 then Inline6 gives back the original source, byte
// for byte, and the original analysis exactly.
func runMR15(t *testing.T, tg *target) *outcome {
	tg.load(t)
	start := time.Now()
	o := &outcome{skipped: map[string]int{}}
	defer func() { o.dur = time.Since(start) }()
	files := tg.files()
	ext, err := tg.ws.Apply(MR6, files, filepath.Join(t.TempDir(), "x"), seed, nil)
	if err != nil {
		t.Fatal(err)
	}
	maps.Copy(o.skipped, ext.Skipped)
	if len(ext.Files) == 0 {
		return o
	}
	ws2, err := LoadWorkspace(ext.Dir)
	if err != nil {
		t.Fatal(err)
	}
	inl, err := ws2.Apply(Inline6, ext.Files, filepath.Join(t.TempDir(), "i"), seed, nil)
	if err != nil {
		t.Fatal(err)
	}
	o.sites = len(inl.Sites)
	for _, rel := range ext.Files {
		a, _ := os.ReadFile(filepath.Join(tg.ws.Dir, rel))
		b, _ := os.ReadFile(filepath.Join(inl.Dir, rel))
		if string(a) != string(b) {
			o.fail("%s: inlining the MR6 helpers does not give back the original source", rel)
		}
	}
	post, err := analyze(inl.Dir, 4, nil)
	if err != nil {
		t.Fatal(err)
	}
	mapped, unmapped := tg.pre.view.Map(Compose(ext.Map, inl.Map))
	if len(unmapped) > 0 {
		o.fail("keys do not map: %s", strings.Join(head(unmapped, 10), ", "))
	}
	for _, d := range Diff(mapped, post.view, Scope{}, 40) {
		o.fail("%s", d)
	}
	return o
}

// ─── harness tests (Step 1) ───────────────────────────────────────────────────

// TestPosMap_RoundTrip: every transform's PosMap maps each old statement and
// call it keeps to a node of the same kind, in the transformed module (which
// type-checks: LoadWorkspace would report its errors).
func TestPosMap_RoundTrip(t *testing.T) {
	tg := fixtures[0]
	tg.load(t)
	for _, tr := range []Transform{MR1, MR2, MR3, MR4, MR5, MR6, MR8, MR9, MR10, MR11, MR12, MR13, MR14} {
		t.Run(tr.Name, func(t *testing.T) {
			t.Parallel()
			res, err := tg.ws.Apply(tr, tg.files(), filepath.Join(t.TempDir(), "m"), seed, tg.pre.view)
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Files) == 0 {
				t.Fatalf("%s changed no file", tr.Name)
			}
			ws2, err := LoadWorkspace(res.Dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, f := range ws2.Files {
				if f.Skip == "package has errors" {
					t.Fatalf("%s: %s does not type-check: %v", tr.Name, f.Rel, f.Pkg.Errors)
				}
			}
			checked := 0
			for _, rel := range res.Files {
				old := tg.ws.Files[rel]
				ast.Inspect(old.AST, func(n ast.Node) bool {
					switch n.(type) {
					case ast.Stmt, *ast.CallExpr:
					default:
						return true
					}
					if _, isBlock := n.(*ast.BlockStmt); isBlock {
						return true
					}
					p, ok := res.Map.Map(old.pos(n.Pos()))
					if !ok {
						return true // deleted or replaced
					}
					nf := ws2.Files[p.File]
					if nf == nil {
						t.Errorf("%s: %v maps to %s, not a file", tr.Name, old.pos(n.Pos()), p.File)
						return true
					}
					want := reflect.TypeOf(n)
					if !nodeAt(nf, p, want) && !kindChanged(tr, n, want) {
						t.Errorf("%s: %s %v maps to %v, which holds no %v", tr.Name, want, old.pos(n.Pos()), p, want)
					}
					checked++
					return true
				})
			}
			if checked == 0 {
				t.Errorf("%s: nothing checked", tr.Name)
			}
		})
	}
}

// kindChanged reports whether tr changes the kind of statement n on
// purpose: MR14 defers a call statement, so the expression statement
// becomes a defer statement.
func kindChanged(tr Transform, n ast.Node, want reflect.Type) bool {
	return tr.Name == "MR14" && want == reflect.TypeOf(&ast.ExprStmt{})
}

// nodeAt reports whether f has a node of type want starting at p.
func nodeAt(f *SrcFile, p Pos, want reflect.Type) bool {
	off, ok := f.lines.offset(p.Line, p.Col)
	if !ok {
		return false
	}
	found := false
	ast.Inspect(f.AST, func(n ast.Node) bool {
		if n == nil || found {
			return false
		}
		if f.off(n.Pos()) == off && reflect.TypeOf(n) == want {
			found = true
		}
		return f.off(n.Pos()) <= off && off < f.off(n.End())+1
	})
	return found
}

// TestCompare_DetectsChange: each oracle flags a hand-made change. An
// untransformed copy compares equal; removing one ddg edge fails the exact
// oracle; a delta naming that removal passes; and removing a param_in edge
// that carried a flow fails the reachability oracle.
func TestCompare_DetectsChange(t *testing.T) {
	tg := fixtures[0]
	tg.load(t)
	pre := tg.pre.view
	cp, err := analyze(copyOf(t, tg.ws.Dir), 4, nil)
	if err != nil {
		t.Fatal(err)
	}
	if d := Diff(pre, cp.view, Scope{}, 10); len(d) > 0 {
		t.Fatalf("an untransformed copy differs: %v", d)
	}

	id, edge := firstEdge(pre, "ddg")
	got := cloneView(pre)
	delete(got.Calls[id].Edges, edge)
	d := Diff(pre, got, Scope{}, 0)
	if len(d) != 1 || !strings.Contains(d[0], edge) || !strings.HasSuffix(d[0], "missing") {
		t.Fatalf("exact oracle on a one-edge removal: %v", d)
	}
	f := strings.Split(edge, "|")
	want, err := Expect(pre, pre, nil, nil, []Delta{{Site: "test", Callable: id,
		Remove: []DEdge{{Kind: "ddg", Src: Pre(f[1]), Dst: Pre(f[2]), Label: f[3], Prov: f[4]}}}})
	if err != nil {
		t.Fatal(err)
	}
	if d := Diff(want, got, Scope{}, 0); len(d) > 0 {
		t.Errorf("delta oracle with the removal declared: %v", d)
	}
	if d := Diff(want, pre, Scope{}, 0); len(d) != 1 {
		t.Errorf("delta oracle with the removal declared, edge kept: %v", d)
	}
	if _, err := Expect(pre, got, nil, nil, []Delta{{Site: "test", Callable: id,
		Remove: []DEdge{{Kind: "ddg", Src: Pre(f[1]), Dst: Pre(f[2]), Label: f[3], Prov: f[4]}}}}); err == nil {
		t.Error("a delta removing an edge the original lacks is not reported")
	}

	// Reachability: drop every param_in edge; some flow is lost.
	cut := cloneView(pre)
	for e := range cut.App {
		if strings.HasPrefix(e, "pin|") {
			delete(cut.App, e)
		}
	}
	if rr := CheckReach(pre, cut, nil, nil, 0, seed); len(rr.Lost) == 0 || rr.Pairs == 0 {
		t.Errorf("reachability oracle without param_in: %d pairs, none lost", rr.Pairs)
	}
	if rr := CheckReach(pre, pre, nil, nil, 0, seed); len(rr.Lost) > 0 || len(rr.Unlocated) > 0 {
		t.Errorf("reachability oracle on itself: lost %v, unlocated %v", head(rr.Lost, 5), head(rr.Unlocated, 5))
	}
}

// TestRefCDG: the analyzer's CDG equals the reference CDG of its CFG on
// every fixture callable (the delta relations derive expected CDGs with it).
func TestRefCDG(t *testing.T) {
	for _, tg := range fixtures {
		tg.load(t)
		n := 0
		for _, id := range slices.Sorted(maps.Keys(tg.pre.view.Calls)) {
			c := tg.pre.view.Calls[id]
			if !c.L3 {
				continue
			}
			n++
			if d := cdgDiff(c, RefCDG(c)); d != "" {
				t.Errorf("%s %s: cdg differs from the reference: %s", tg.name, id, d)
			}
		}
		if n < 50 {
			t.Errorf("%s: only %d callables", tg.name, n)
		}
	}
}

func firstEdge(v *View, kind string) (string, string) {
	for _, id := range slices.Sorted(maps.Keys(v.Calls)) {
		for _, e := range slices.Sorted(maps.Keys(v.Calls[id].Edges)) {
			if strings.HasPrefix(e, kind+"|") {
				return id, e
			}
		}
	}
	return "", ""
}

func cloneView(v *View) *View {
	out := &View{Calls: map[string]*CV{}, App: maps.Clone(v.App)}
	for id, c := range v.Calls {
		out.Calls[id] = c.clone()
	}
	return out
}

func copyOf(t *testing.T, dir string) string {
	dst := filepath.Join(t.TempDir(), "copy")
	if err := copyTree(dir, dst); err != nil {
		t.Fatal(err)
	}
	return dst
}

// ─── one relation per oracle, on one named fixture site ───────────────────────
//
// The table above runs every relation on whole samples. These three pin one
// site each, so a relation's shape reads end to end.

// TestMR2_ReverseDeclarations_Exact: reversing sdg/stdlib.go's declarations
// leaves the -a 4 analysis exactly as it was, once every key is moved to its
// statement's new line.
func TestMR2_ReverseDeclarations_Exact(t *testing.T) {
	tg := fixtures[0]
	tg.load(t)
	res, post := transformAndAnalyze(t, tg, MR2, tg.ws, []string{"sdg/stdlib.go"}, tg.pre.view)
	mapped, unmapped := tg.pre.view.Map(res.Map)
	if len(unmapped) > 0 {
		t.Fatalf("keys do not map: %v", unmapped)
	}
	if d := Diff(mapped, post.view, Scope{}, 20); len(d) > 0 {
		t.Errorf("MR2 changed the analysis:\n%s", strings.Join(d, "\n"))
	}
}

// TestMR9_InsertKill_Delta: in ScalarSink, inserting `x = 0` before
// `sinkInt(x)` moves every def edge of `x := a + 1` on x to the kill and
// splices the kill into the CFG; the CDG is the reference CDG of that CFG,
// and nothing else in the application changes.
func TestMR9_InsertKill_Delta(t *testing.T) {
	tg := fixtures[0]
	tg.load(t)
	res, post := transformAndAnalyze(t, tg, MR9, tg.ws, []string{"sdg/sdg.go"}, tg.pre.view)
	var site *Site
	for i, s := range res.Sites {
		if strings.HasSuffix(s.Callable, ".ScalarSink") {
			site = &res.Sites[i]
		}
	}
	if site == nil {
		t.Fatal("MR9 chose no site in ScalarSink")
	}
	var k Ref // the kill, a node the transform adds (its one mark)
	for m := range site.Delta.Nodes {
		k = Mark(m)
	}
	want := Delta{Site: site.Desc, Callable: site.Callable, CDGFromCFG: true,
		Nodes: site.Delta.Nodes,
		Remove: []DEdge{
			DDG(Pre("509:2"), Pre("510:2"), "x"),
			DDG(Pre("509:2"), Pre("511:2"), "x"),
			CFG(Pre("509:2"), Pre("510:2"), "fallthrough"),
		},
		Add: []DEdge{
			DDG(k, Pre("510:2"), "x"),
			DDG(k, Pre("511:2"), "x"),
			CFG(Pre("509:2"), k, "fallthrough"),
			CFG(k, Pre("510:2"), "fallthrough"),
		},
	}
	mapped, _ := tg.pre.view.Map(res.Map)
	var ds []Delta
	for _, s := range res.Sites {
		if s.Callable == want.Callable {
			ds = append(ds, want)
		} else {
			ds = append(ds, *s.Delta)
		}
	}
	exp, err := Expect(tg.pre.view, mapped, res.Map, res.Marks, ds)
	if err != nil {
		t.Fatal(err)
	}
	if d := Diff(exp, post.view, Scope{}, 20); len(d) > 0 {
		t.Errorf("MR9 delta:\n%s", strings.Join(d, "\n"))
	}
}

// TestMR5_IntroduceAlias_Reach: rewriting `q.n = v` in aliasAssign as
// `mr5q := q; mr5q.n = v` (an alias of an alias) keeps every flow from the
// formal_ins of aliasAssign and of its caller CallAliasAssign: v still
// reaches the formal_out of p.
func TestMR5_IntroduceAlias_Reach(t *testing.T) {
	tg := fixtures[0]
	tg.load(t)
	res, post := transformAndAnalyze(t, tg, MR5, tg.ws, []string{"sdg/sdg.go"}, tg.pre.view)
	f := "example.com/dataflow/sdg.aliasAssign"
	if !slices.Contains(res.Touched, f) {
		t.Fatalf("MR5 did not rewrite %s", f)
	}
	rr := CheckReach(tg.pre.view, post.view, res.Map, []string{f, "example.com/dataflow/sdg.CallAliasAssign"}, 2, seed)
	if rr.Pairs == 0 {
		t.Fatal("no flow checked")
	}
	if len(rr.Lost) > 0 || len(rr.Unlocated) > 0 {
		t.Errorf("MR5: lost %v, unlocated %v", rr.Lost, rr.Unlocated)
	}
	// The flow the alias carries: v (formal_in 1) → p's formal_out.
	g := post.view.Graph(true)
	if !g.Reach(Node(f, "@formal_in:1"))[Node(f, "@formal_out")] {
		t.Error("after MR5, v no longer reaches aliasAssign's formal_out for p")
	}
}
