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

// examples is testdata/metamorphic: small functions written to be eligible
// sites of every relation, and the convention pins (conventions.go).
var examples = &target{name: "examples", dir: testdata("metamorphic")}

var fixtures = []*target{
	{name: "dataflow", dir: testdata("dataflow"), pkgDirs: []string{"flow", "sdg"}},
	{name: "chi", dir: testdata("chi"), nFiles: 12, nCallables: 60},
	examples,
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
	for _, v := range res.Violations {
		o.fail("%s", v)
	}
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
	// A precision relation: nothing is added anywhere. At a site, its
	// declared removals may go (CheckAtMost). A summary edge may go only in
	// a site callable or one of its transitive callers (over param_in):
	// the constant cuts a flow of the site callable, which changes its own
	// summary edges and those of every caller whose summary is derived from
	// its flows, and nothing else.
	sumOK := map[string]bool{}
	for id := range atMost {
		maps.Copy(sumOK, pre.Callers(id))
	}
	for _, d := range Diff(want, got, Scope{Skip: atMost}, 0) {
		if id, rest, ok := strings.Cut(d, ": edge sum|"); ok && strings.HasSuffix(rest, " missing") && sumOK[id] {
			continue
		}
		o.fail("%s", d)
	}
	for _, d := range CheckAtMost(pre, mapped, got, res.Map, res.Marks, ds) {
		// A site callable is its own caller set's root, so its summary
		// edges may go too.
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
	if len(res.Sites) > 0 && rr.Pairs == 0 {
		o.fail("%d sites but no source-sink pair checked: the oracle checked nothing", len(res.Sites))
	}
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
		floor := 50
		if tg == examples {
			floor = 30 // a small module of focused examples
		}
		if n < floor {
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

// ─── one relation per oracle, on one named example ────────────────────────────
//
// The table above runs every relation on whole samples. These three pin one
// site each on testdata/metamorphic, so a relation's shape reads end to end.
// Keys are found by statement text (stmtKey), never by line number.

const exPkg = "example.com/metamorphic."

// stmtKey is the body key of the one statement of function fn in examples
// file rel whose text is text, or starts with text followed by a token
// boundary (so "x := a + 1" never matches "x := a + 10"). Zero or several
// matches fail the test.
func stmtKey(t *testing.T, rel, fn, text string) string {
	t.Helper()
	f := examples.ws.Files[rel]
	if f == nil {
		t.Fatalf("no file %s", rel)
	}
	var keys []string
	for _, fd := range funcDecls(f) {
		if fd.Name.Name != fn {
			continue
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			st, ok := n.(ast.Stmt)
			if !ok {
				return true
			}
			if _, block := st.(*ast.BlockStmt); block {
				return true
			}
			if src := string(f.Src[f.off(st.Pos()):f.off(st.End())]); textMatch(src, text) {
				keys = append(keys, f.key(st.Pos()))
			}
			return true
		})
	}
	if len(keys) != 1 {
		t.Fatalf("%s: %d statements %q in %s (%v), want exactly one", rel, len(keys), text, fn, keys)
	}
	return keys[0]
}

// textMatch reports whether src is text, or text then a token boundary: a
// character that cannot continue text's last token.
func textMatch(src, text string) bool {
	if !strings.HasPrefix(src, text) {
		return false
	}
	if len(src) == len(text) {
		return true
	}
	word := func(c byte) bool {
		return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
	}
	return !(word(text[len(text)-1]) && word(src[len(text)]))
}

// siteIn is the site of res in callable id.
func siteIn(t *testing.T, res *Result, id string) Site {
	t.Helper()
	for _, s := range res.Sites {
		if s.Callable == id {
			return s
		}
	}
	t.Fatalf("no site in %s", id)
	return Site{}
}

// TestMR2_ReverseDeclarations_Exact: reversing mr01_04_layout.go's
// declarations (the method Scale goes back below its type) leaves the -a 4
// analysis exactly as it was, once every key moves to its statement's new
// line.
func TestMR2_ReverseDeclarations_Exact(t *testing.T) {
	tg := examples
	tg.load(t)
	res, post := transformAndAnalyze(t, tg, MR2, tg.ws, []string{"mr01_04_layout.go"}, tg.pre.view)
	mapped, unmapped := tg.pre.view.Map(res.Map)
	if len(unmapped) > 0 {
		t.Fatalf("keys do not map: %v", unmapped)
	}
	if d := Diff(mapped, post.view, Scope{}, 20); len(d) > 0 {
		t.Errorf("MR2 changed the analysis:\n%s", strings.Join(d, "\n"))
	}
}

// TestMR9_InsertKill_Delta: in KillOneUse, inserting `x = 0` before
// `sinkInt(x)` moves both def edges of `x := a + 1` on x (to the call and
// to the return) to the kill and splices the kill into the CFG; the CDG is
// the reference CDG of that CFG, and nothing else in the module changes.
func TestMR9_InsertKill_Delta(t *testing.T) {
	tg := examples
	tg.load(t)
	res, post := transformAndAnalyze(t, tg, MR9, tg.ws, []string{"mr09_kill.go"}, tg.pre.view)
	site := siteIn(t, res, exPkg+"KillOneUse")
	var k Ref // the kill, the node the transform adds (its one mark)
	for m := range site.Delta.Nodes {
		k = Mark(m)
	}
	d := stmtKey(t, "mr09_kill.go", "KillOneUse", "x := a + 1")
	u := stmtKey(t, "mr09_kill.go", "KillOneUse", "sinkInt(x)")
	r := stmtKey(t, "mr09_kill.go", "KillOneUse", "return x")
	want := Delta{Site: site.Desc, Callable: site.Callable, CDGFromCFG: true, Nodes: site.Delta.Nodes,
		Remove: []DEdge{
			DDG(Pre(d), Pre(u), "x"),
			DDG(Pre(d), Pre(r), "x"),
			CFG(Pre(d), Pre(u), "fallthrough"),
		},
		Add: []DEdge{
			DDG(k, Pre(u), "x"),
			DDG(k, Pre(r), "x"),
			CFG(Pre(d), k, "fallthrough"),
			CFG(k, Pre(u), "fallthrough"),
		},
	}
	mapped, unmapped := tg.pre.view.Map(res.Map)
	if len(unmapped) > 0 {
		t.Fatalf("keys do not map: %v", unmapped)
	}
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
	if diff := Diff(exp, post.view, Scope{}, 20); len(diff) > 0 {
		t.Errorf("MR9 delta:\n%s", strings.Join(diff, "\n"))
	}
}

// TestMR5_IntroduceAlias_Reach: rewriting `p.n = v` in SetPtr as
// `q := p; q.n = v` keeps every flow from SetPtr's formal_ins: v still
// reaches the formal_out of p, now through the alias oracle.
func TestMR5_IntroduceAlias_Reach(t *testing.T) {
	tg := examples
	tg.load(t)
	res, post := transformAndAnalyze(t, tg, MR5, tg.ws, []string{"mr05_alias.go"}, tg.pre.view)
	f := exPkg + "SetPtr"
	if !slices.Contains(res.Touched, f) {
		t.Fatalf("MR5 did not rewrite %s", f)
	}
	rr := CheckReach(tg.pre.view, post.view, res.Map, []string{f}, 1, seed)
	if rr.Pairs == 0 {
		t.Fatal("no flow checked")
	}
	if len(rr.Lost) > 0 || len(rr.Unlocated) > 0 {
		t.Errorf("MR5: lost %v, unlocated %v", rr.Lost, rr.Unlocated)
	}
	// The flow the alias carries: v (formal_in 1) → p's formal_out.
	if !post.view.Graph(true).Reach(Node(f, "@formal_in:1"))[Node(f, "@formal_out")] {
		t.Error("after MR5, v no longer reaches SetPtr's formal_out for p")
	}
}

// ─── the examples module ──────────────────────────────────────────────────────

// TestExamples_EveryRelationHasASite: every relation MR1-MR15 transforms at
// least one site of testdata/metamorphic, and the relations with a file of
// their own land on the functions written for them: MR13 on mr13_*, each
// MR14 form (a interface, b closure, c defer) on mr14_*, MR7 exact on a
// named file, MR15 byte-identical on the MR6 example.
func TestExamples_EveryRelationHasASite(t *testing.T) {
	tg := examples
	tg.load(t)
	sitesIn := func(res *Result, file string) []Site {
		var out []Site
		for _, s := range res.Sites {
			if strings.HasPrefix(s.Desc, file+":") {
				out = append(out, s)
			}
		}
		return out
	}
	for _, r := range relations {
		res, err := tg.ws.Apply(r.t, tg.files(), filepath.Join(t.TempDir(), r.name), seed, tg.pre.view)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s: %d sites", r.name, len(res.Sites))
		if len(res.Sites) == 0 {
			t.Errorf("%s has no site on the examples module (skipped: %v)", r.name, res.Skipped)
		}
		switch r.name {
		case "MR13":
			want := []string{"ConstScalar", "ConstTwoArgs", "ConstInLoop"}
			for _, fn := range want {
				if !slices.ContainsFunc(sitesIn(res, "mr13_const_sink.go"), func(s Site) bool { return s.Callable == exPkg+fn }) {
					t.Errorf("MR13 has no site in %s (mr13_const_sink.go)", fn)
				}
			}
		case "MR14":
			forms := map[string]string{ // the function written for each form → its Desc
				"RouteIface":   "call sinkInt through mr14i",
				"RouteClosure": "call through a closure",
				"RouteDefer":   "defer the call",
			}
			for fn, form := range forms {
				if !slices.ContainsFunc(sitesIn(res, "mr14_route.go"), func(s Site) bool {
					return s.Callable == exPkg+fn && strings.Contains(s.Desc, form)
				}) {
					t.Errorf("MR14 form %q has no site in %s (mr14_route.go)", form, fn)
				}
			}
		}
	}

	// MR7: --target-files on mr09_kill.go at -a 3 is exact on its callables.
	full := tg.level3(t)
	const rel7 = "mr09_kill.go"
	part, err := analyze(tg.ws.Dir, 3, []string{filepath.Join(tg.ws.Dir, rel7)})
	if err != nil {
		t.Fatal(err)
	}
	only := map[string]bool{}
	for id, c := range full.view.Calls {
		if c.File == rel7 {
			only[id] = true
		}
	}
	if len(only) < 3 {
		t.Fatalf("MR7: only %d callables in %s", len(only), rel7)
	}
	if d := Diff(full.view, part.view, Scope{Only: only}, 20); len(d) > 0 {
		t.Errorf("MR7 on %s:\n%s", rel7, strings.Join(d, "\n"))
	}

	// MR15: MR6 extracts a helper from Extract, and Inline6 gives
	// mr06_extract.go back byte for byte.
	const rel15 = "mr06_extract.go"
	ext, err := tg.ws.Apply(MR6, []string{rel15}, filepath.Join(t.TempDir(), "mr15x"), seed, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(ext.Sites, func(s Site) bool { return s.Callable == exPkg+"Extract" }) {
		t.Fatalf("MR15: MR6 extracted nothing from Extract: %v", ext.Skipped)
	}
	mid, _ := os.ReadFile(filepath.Join(ext.Dir, rel15))
	if !strings.Contains(string(mid), "func "+Prefix6) {
		t.Fatalf("MR15: %s has no MR6 helper after the extraction", rel15)
	}
	ws2, err := LoadWorkspace(ext.Dir)
	if err != nil {
		t.Fatal(err)
	}
	inl, err := ws2.Apply(Inline6, []string{rel15}, filepath.Join(t.TempDir(), "mr15i"), seed, nil)
	if err != nil {
		t.Fatal(err)
	}
	orig, _ := os.ReadFile(filepath.Join(tg.ws.Dir, rel15))
	back, _ := os.ReadFile(filepath.Join(inl.Dir, rel15))
	if string(orig) != string(back) {
		t.Errorf("MR15: inlining does not restore %s byte for byte:\n--- original\n%s\n--- inlined\n%s", rel15, orig, back)
	}
}

// ─── convention pins (testdata/metamorphic/conventions.go) ────────────────────
//
// Task 17b encoded four analyzer conventions as oracle rules. Each test pins
// the analyzer fact its rule rests on, so a change that invalidates the rule
// fails here first, naming it, instead of silently weakening a relation.

// TestConvention_CaptureSiteActualInIsNotASink pins rule (i), the
// reachability oracle's sink set: a capture site's actual_in has exactly
// one out-edge, the param_in to its closure's capture formal_in (no ddg,
// no summary), so it is not a sink. A call node's actual_in is one.
func TestConvention_CaptureSiteActualInIsNotASink(t *testing.T) {
	const rule = "rule (i), capture-site actual_ins are not reachability sinks (CheckReach, View.isSink)"
	tg := examples
	tg.load(t)
	v := tg.pre.view
	id := exPkg + "ConvCaptureSite"
	site := stmtKey(t, "conventions.go", "ConvCaptureSite", "f := func()") // the literal's host
	litKey := ""
	for k, b := range v.Calls[id].Body {
		if strings.HasSuffix(k, "/actual_in:0") && strings.Split(b, "|")[1] == site {
			litKey = strings.TrimSuffix(k, "/actual_in:0")
		}
	}
	if litKey == "" {
		t.Fatalf("%s: ConvCaptureSite has no capture site hosted at %s", rule, site)
	}
	ain := Node(id, litKey+"/actual_in:0")
	g := v.Graph(false)
	succ := g[ain]
	if len(succ) != 1 || !strings.HasPrefix(succ[0], id+".closure_1|@formal_in:") {
		t.Errorf("%s: %s's out-edges are %v, want exactly the param_in to closure_1's capture formal_in", rule, ain, succ)
	}
	if v.isSink(ain) {
		t.Errorf("%s: %s is treated as a sink", rule, ain)
	}
	call := Node(exPkg+"KillOneUse", stmtKey(t, "mr09_kill.go", "KillOneUse", "sinkInt(x)")+"/actual_in:0")
	if !v.isSink(call) {
		t.Errorf("%s: the call actual_in %s is not a sink", rule, call)
	}
}

// TestConvention_GuardSelfLoopOnlyInLoops pins rule (ii), MR8's guard edge:
// the header `if c := true; c` folds init and condition into one node, so
// the guard c adds the ddg self-loop B→B on c exactly when the wrapped
// statement is on a CFG cycle (here, in a loop body), and no edge in
// straight-line code.
func TestConvention_GuardSelfLoopOnlyInLoops(t *testing.T) {
	const rule = "rule (ii), MR8's guard self-loop B→B on c iff the wrapped statement is in a loop"
	tg := examples
	tg.load(t)
	res, post := transformAndAnalyze(t, tg, MR8, tg.ws, []string{"conventions.go"}, tg.pre.view)
	for fn, wantLoop := range map[string]bool{"ConvGuardInLoop": true, "ConvGuardStraight": false} {
		id := exPkg + fn
		s := siteIn(t, res, id)
		var b string
		for m := range s.Delta.Nodes {
			b = res.Marks[m].Key()
		}
		var guard []string
		for e := range post.view.Calls[id].Edges {
			if f := strings.Split(e, "|"); f[0] == "ddg" && strings.HasPrefix(f[3], "mr8c") {
				guard = append(guard, e)
			}
		}
		// The guard exists: B is a branch node, its source text is the
		// `if <c> := true; <c> {` header MR8 wrote, and B → the wrapped
		// statement is its true edge. Its variable is read from that text,
		// so the no-edge check below cannot pass for a renamed guard.
		if a := post.view.Calls[id].Body[b]; !strings.HasPrefix(a, "branch|") {
			t.Fatalf("%s: %s: node %s is %q, want the guard branch", rule, fn, b, a)
		}
		g := guardVar(t, res, "conventions.go", b)
		var on []string
		for e := range post.view.Calls[id].Edges {
			if f := strings.Split(e, "|"); f[0] == "ddg" && f[3] == g {
				on = append(on, e)
			}
		}
		if len(guard) != len(on) {
			t.Errorf("%s: %s: guard edges %v are not the edges on %s, %v", rule, fn, guard, g, on)
		}
		switch {
		case wantLoop && (len(on) != 1 || on[0] != "ddg|"+b+"|"+b+"|"+g+"|ssa"):
			t.Errorf("%s: %s: edges on %s are %v, want the one self-loop on %s", rule, fn, g, on, b)
		case !wantLoop && len(on) != 0:
			t.Errorf("%s: %s: edges on %s are %v, want none", rule, fn, g, on)
		}
	}
}

// guardVar is the variable of the `if <c> := true; <c> {` header MR8 wrote
// at body key b of file rel in the transformed module res.
func guardVar(t *testing.T, res *Result, rel, b string) string {
	t.Helper()
	var p Pos
	for _, m := range res.Marks {
		if m.File == rel && m.Key() == b {
			p = m
		}
	}
	src, err := os.ReadFile(filepath.Join(res.Dir, p.File))
	if err != nil {
		t.Fatal(err)
	}
	off, ok := newLineTable(src).offset(p.Line, p.Col)
	if !ok {
		t.Fatalf("no position %v", p)
	}
	head := string(src[off:])
	var c string
	if _, err := fmt.Sscanf(head, "if %s := true;", &c); err != nil || !strings.HasPrefix(head, "if "+c+" := true; "+c+" {") {
		t.Fatalf("no guard header at %v: %.40q", p, head)
	}
	return c
}

// TestConvention_DiscardedResultReachesOnlyViaHost pins rule (iii), MR14(b)'s
// cut: a discarded result's actual_out has the one out-edge actual_out →
// call node (the coarse host wiring, spec §4.7), and the call node reaches
// the rest of the callable only by control dependence (it may panic, an
// exception edge), so the closure form may lose that path.
func TestConvention_DiscardedResultReachesOnlyViaHost(t *testing.T) {
	const rule = "rule (iii), MR14(b) cuts a discarded <return> actual_out (reaches on only via host cdg)"
	tg := examples
	tg.load(t)
	c := tg.pre.view.Calls[exPkg+"ConvDiscardedResult"]
	call := stmtKey(t, "conventions.go", "ConvDiscardedResult", "p.sum()")
	ret := stmtKey(t, "conventions.go", "ConvDiscardedResult", "return p.a")
	out := edgesOf(c, "ddg")
	from := filter(out, func(e edgeRec) bool { return e.src == call+"/actual_out" })
	if len(from) != 1 || from[0].dst != call || from[0].label != "<return>" {
		t.Errorf("%s: %s/actual_out edges %v, want only → the call node on <return>", rule, call, from)
	}
	if !c.Edges["cdg|"+call+"|"+ret+"|"] {
		t.Errorf("%s: no cdg %s → %s (the call may panic)", rule, call, ret)
	}
	if len(filter(out, func(e edgeRec) bool { return e.src == call && e.dst == ret })) != 0 {
		t.Errorf("%s: the call node has a ddg edge to the return: the result is not discarded", rule)
	}
}

// TestConvention_NamedResultPortMovesToKill pins rule (iv), MR9 on a named
// result: the def `n = a + 1` reaches @formal_out through the @exit port
// edge (reaching-defs, var <return>), and the kill MR9 inserts before the
// return takes it over.
func TestConvention_NamedResultPortMovesToKill(t *testing.T) {
	const rule = "rule (iv), MR9's kill takes over a named result's D → @formal_out port edge"
	tg := examples
	tg.load(t)
	id := exPkg + "ConvNamedKill"
	d := stmtKey(t, "conventions.go", "ConvNamedKill", "n = a + 1")
	port := "ddg|" + d + "|@formal_out|<return>|reaching-defs"
	if !tg.pre.view.Calls[id].Edges[port] {
		t.Fatalf("%s: no port edge %s before the kill", rule, port)
	}
	res, post := transformAndAnalyze(t, tg, MR9, tg.ws, []string{"conventions.go"}, tg.pre.view)
	s := siteIn(t, res, id)
	var k string
	for m := range s.Delta.Nodes {
		k = res.Marks[m].Key()
	}
	nd, _, ok := (&mapper{pm: res.Map}).key("conventions.go", d)
	if !ok {
		t.Fatalf("%s: %s does not map", rule, d)
	}
	edges := post.view.Calls[id].Edges
	if edges["ddg|"+nd+"|@formal_out|<return>|reaching-defs"] {
		t.Errorf("%s: the def %s still reaches @formal_out past the kill", rule, nd)
	}
	if !edges["ddg|"+k+"|@formal_out|<return>|reaching-defs"] {
		t.Errorf("%s: the kill %s has no port edge to @formal_out", rule, k)
	}
}

// TestStmtKey_TokenBoundary: stmtKey's text match needs the whole text or a
// token boundary after it, so one statement prefix never matches a longer
// literal or identifier.
func TestStmtKey_TokenBoundary(t *testing.T) {
	for _, c := range []struct {
		src, text string
		want      bool
	}{
		{"x := a + 1", "x := a + 1", true},
		{"x := a + 10", "x := a + 1", false},
		{"x := a + 1 // c", "x := a + 1", true},
		{"sinkInt(x)", "sinkInt(x)", true},
		{"sinkInt(x)", "sinkInt(", true},
		{"return xy", "return x", false},
		{"return x * x", "return x", true},
	} {
		if got := textMatch(c.src, c.text); got != c.want {
			t.Errorf("textMatch(%q, %q) = %v, want %v", c.src, c.text, got, c.want)
		}
	}
}
