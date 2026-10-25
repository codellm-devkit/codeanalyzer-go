package dataflow_test

import (
	"go/ast"
	"go/token"
	"go/types"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"testing"

	"github.com/codellm-devkit/codeanalyzer-go/internal/dataflow"
)

func dd(src, dst, v string) dataflow.DdgEdge { return dataflow.DdgEdge{Src: src, Dst: dst, Var: v} }

// duMap is DefUseOf for every node of the pruned set, keyed by node Key —
// BuildDDG's input, computed the way the analyzer computes it.
func duMap(nodes []dataflow.Node, info *types.Info, fn ast.Node, keyOf map[ast.Node]string) map[string]dataflow.DefUse {
	bases := dataflow.EntryBases(fn, info)
	out := map[string]dataflow.DefUse{}
	for _, n := range nodes {
		out[n.Key] = dataflow.DefUseOf(n, info, kTest, bases, fn, keyOf)
	}
	return out
}

// ddgOf builds fn's pruned nodes, CFG, CDG and DDG from the flow fixture with
// its real L1 call keys.
func ddgOf(t *testing.T, fn string) (kept []dataflow.Node, cdg []dataflow.CdgEdge, ddg []dataflow.DdgEdge) {
	t.Helper()
	fx := loadFlow(t)
	d := fx.decl(t, fn)
	nodes, keyOf := dataflow.BuildNodes(fx.fset, d.Body, fx.callKeys(t, fn))
	kept, es := dataflow.BuildCFG(nodes, keyOf, d.Body, fx.info)
	checkContract(t, fn, kept, es)
	ddg = dataflow.BuildDDG(kept, es, duMap(kept, fx.info, d, keyOf))
	checkDDG(t, fn, kept, ddg)
	return kept, dataflow.BuildCDG(kept, es), ddg
}

// checkDDG is the output contract: every endpoint is a pruned node and never
// a satellite call node (R13); sorted by (Src, Dst, Var), no duplicates;
// non-nil.
func checkDDG(t *testing.T, fn string, nodes []dataflow.Node, ddg []dataflow.DdgEdge) {
	t.Helper()
	if ddg == nil {
		t.Errorf("%s: BuildDDG returned nil", fn)
	}
	byKey := map[string]dataflow.Node{}
	for _, n := range nodes {
		byKey[n.Key] = n
	}
	for _, g := range ddg {
		for _, k := range []string{g.Src, g.Dst} {
			n, ok := byKey[k]
			if !ok {
				t.Errorf("%s: ddg %v endpoint %s is not a pruned node", fn, g, k)
			} else if n.Parent != "" {
				t.Errorf("%s: ddg %v links satellite call %s (R13)", fn, g, k)
			}
		}
		if g.Var == "" {
			t.Errorf("%s: ddg %v has no var", fn, g)
		}
	}
	for i := 1; i < len(ddg); i++ {
		a, b := ddg[i-1], ddg[i]
		if !ddgLess(a, b) {
			t.Errorf("%s: ddg out of order or duplicated: %v before %v", fn, a, b)
		}
	}
}

func ddgLess(a, b dataflow.DdgEdge) bool {
	if a.Src != b.Src {
		return a.Src < b.Src
	}
	if a.Dst != b.Dst {
		return a.Dst < b.Dst
	}
	return a.Var < b.Var
}

// wantDDG asserts fn's DDG is exactly want.
func wantDDG(t *testing.T, fn string, want ...dataflow.DdgEdge) []dataflow.DdgEdge {
	t.Helper()
	_, _, got := ddgOf(t, fn)
	have := map[dataflow.DdgEdge]bool{}
	for _, g := range got {
		have[g] = true
	}
	exp := map[dataflow.DdgEdge]bool{}
	for _, w := range want {
		exp[w] = true
		if !have[w] {
			t.Errorf("%s: missing ddg %s→%s on %q", fn, w.Src, w.Dst, w.Var)
		}
	}
	for _, g := range got {
		if !exp[g] {
			t.Errorf("%s: unexpected ddg %s→%s on %q", fn, g.Src, g.Dst, g.Var)
		}
	}
	return got
}

// Derivations below use the CFGs pinned in cfg_test.go and the DefUse sets
// pinned in defuse_test.go. IN(n) = ∪ OUT(pred); a node's uses read IN(n)
// (before its own defs); OUT(n) = GEN(n) ∪ (IN(n) − KILL(n)), KILL by the
// node's strong defs only.

func TestDDG_LoopCarried(t *testing.T) {
	// Bump. @entry D n; 146:2 D y; 147:2 (for header, Init and Post both
	// define i: strong) D i U i,n; 148:3 D y U y; 150:2 U y.
	// CFG: @entry→146:2→147:2; 147:2→148:3, 147:2→150:2; 148:3→147:2.
	// IN(147:2) = OUT(146:2) ∪ OUT(148:3) = {n@entry, y@146, i@147, y@148}.
	//   i ← 147:2 (the post's i++ across the back edge), n ← @entry.
	// IN(148:3) = OUT(147:2) = same set: y ← 146:2 (first trip) and 148:3
	//   itself (loop-carried: its use reads IN, before its own def).
	// IN(150:2) = OUT(147:2): y ← 146:2 (zero trips), 148:3.
	wantDDG(t, "Bump",
		dd(entry, "147:2", "n"),
		dd("146:2", "148:3", "y"),
		dd("146:2", "150:2", "y"),
		dd("147:2", "147:2", "i"),
		dd("148:3", "148:3", "y"),
		dd("148:3", "150:2", "y"),
	)
}

func TestDDG_ShadowedVariableDoesNotLeak(t *testing.T) {
	// Shadow. 155:2 x(outer) := a; 157:3 x(inner) := a*2; 158:3 _ = x(inner);
	// 160:2 return x(outer). The two x are distinct bases: the inner def
	// reaches 160:2 (157:3→158:3→160:2) but does not interfere with the
	// outer use, and the outer def never feeds 158:3.
	got := wantDDG(t, "Shadow",
		dd(entry, "155:2", "a"),
		dd(entry, "156:2", "a"),
		dd(entry, "157:3", "a"),
		dd("155:2", "160:2", "x"),
		dd("157:3", "158:3", "x"),
	)
	for _, g := range got {
		if g.Src == "157:3" && g.Dst == "160:2" {
			t.Errorf("Shadow: inner x leaks to the outer use: %v", g)
		}
	}
}

func TestDDG_FieldPrefix(t *testing.T) {
	// Fields. @entry D p, v; 176:2 (p.f.g.h.i = v, past k=3) W p.f.g.* U p, v;
	// 177:2 U p.f.g.x. The weak p.f.g.* covers p.f.g.x; the @entry def p is a
	// prefix of it and survives (a weak def kills nothing).
	wantDDG(t, "Fields",
		dd(entry, "176:2", "p"),
		dd(entry, "176:2", "v"),
		dd(entry, "177:2", "p.f.g.x"),
		dd("176:2", "177:2", "p.f.g.x"),
	)
}

func TestDDG_DeferArgsFlowToExit(t *testing.T) {
	// EarlyReturnDefer. Each registration weak-defines "<defer>:<key>" of its
	// deferred call node, which uses it; it reaches the chain along every
	// exit path. a's args evaluate at registration (127:2 U a); the nested
	// satellite g(a) (127:13) carries nothing and is never an endpoint.
	// The pseudo paths compare by full spelling: <defer>:126:8 does not feed
	// 127:8.
	got := wantDDG(t, "EarlyReturnDefer",
		dd(entry, "127:2", "a"),
		dd(entry, "128:2", "a"),
		dd(entry, "132:2", "a"),
		dd("126:2", "126:8", "<defer>:126:8"),
		dd("127:2", "127:8", "<defer>:127:8"),
	)
	for _, g := range got {
		if g.Src == "127:13" || g.Dst == "127:13" {
			t.Errorf("EarlyReturnDefer: satellite 127:13 is a ddg endpoint: %v", g)
		}
	}
}

func TestDDG_Chan(t *testing.T) {
	// Chan. @entry D ch, v; 182:2 (ch <- v) W ch[*] U ch, v; 183:2 (w := <-ch)
	// D w U ch, ch[*]; 184:2 U w.
	// Use ch[*] at 183:2: 182:2's ch[*] (equal) and @entry's ch (a prefix).
	// Use ch at 183:2: @entry's ch (equal) and 182:2's ch[*] (ch is a prefix
	// of it — interference is symmetric). One edge per use spelling.
	wantDDG(t, "Chan",
		dd(entry, "182:2", "ch"),
		dd(entry, "182:2", "v"),
		dd(entry, "183:2", "ch"),
		dd(entry, "183:2", "ch[*]"),
		dd("182:2", "183:2", "ch"),
		dd("182:2", "183:2", "ch[*]"),
		dd("183:2", "184:2", "w"),
	)
}

// TestDDG_KillRules locks gen/kill on a hand-built straight line, with no
// fixture in between:
//
//	@entry  D x.f, x.g
//	1:1     D x.f                  kills @entry's x.f only
//	1:2     W x.f, x.h.*           kills nothing (1:1's x.f survives)
//	1:3     U x                    ← @entry x.g, 1:1 x.f, 1:2 x.f / x.h.*
//	1:4     D x  U x               uses read IN first: same three; then
//	                               x kills every x… def (x covers them)
//	1:5     U x.f                  ← 1:4 only (x is a prefix of x.f)
//	1:6     D x.f.g  U x.f         a field def does not kill x: 1:4 only
//	1:7     U x                    ← 1:4 x, 1:6 x.f.g
func TestDDG_KillRules(t *testing.T) {
	x := types.NewVar(token.NoPos, nil, "x", types.Typ[types.Int])
	P := func(s ...string) []dataflow.Path {
		var out []dataflow.Path
		for _, sp := range s {
			out = append(out, dataflow.Path{Base: x, Spell: sp})
		}
		return out
	}
	keys := []string{entry, "1:1", "1:2", "1:3", "1:4", "1:5", "1:6", "1:7", exit}
	var nodes []dataflow.Node
	var cfg []dataflow.CfgEdge
	for i, k := range keys {
		nodes = append(nodes, dataflow.Node{Key: k})
		if i > 0 {
			cfg = append(cfg, dataflow.CfgEdge{Src: keys[i-1], Dst: k, Kind: "fallthrough"})
		}
	}
	du := map[string]dataflow.DefUse{
		entry: {Defs: P("x.f", "x.g")},
		"1:1": {Defs: P("x.f")},
		"1:2": {WeakDefs: P("x.f", "x.h.*")},
		"1:3": {Uses: P("x")},
		"1:4": {Defs: P("x"), Uses: P("x")},
		"1:5": {Uses: P("x.f")},
		"1:6": {Defs: P("x.f.g"), Uses: P("x.f")},
		"1:7": {Uses: P("x")},
	}
	got := dataflow.BuildDDG(nodes, cfg, du)
	want := []dataflow.DdgEdge{
		dd("1:1", "1:3", "x"),
		dd("1:1", "1:4", "x"),
		dd("1:2", "1:3", "x"),
		dd("1:2", "1:4", "x"),
		dd("1:4", "1:5", "x.f"),
		dd("1:4", "1:6", "x.f"),
		dd("1:4", "1:7", "x"),
		dd("1:6", "1:7", "x"),
		dd(entry, "1:3", "x"),
		dd(entry, "1:4", "x"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("kill rules:\n got %v\nwant %v", got, want)
	}
}

// sliceOf is the backward intraprocedural slice from criterion: reverse
// reachability over cdg ∪ ddg, criterion and @entry included, @exit excluded.
func sliceOf(cdg []dataflow.CdgEdge, ddg []dataflow.DdgEdge, criterion string) []string {
	pred := map[string][]string{}
	for _, e := range cdg {
		pred[e.Dst] = append(pred[e.Dst], e.Src)
	}
	for _, e := range ddg {
		pred[e.Dst] = append(pred[e.Dst], e.Src)
	}
	seen := map[string]bool{criterion: true}
	work := []string{criterion}
	for len(work) > 0 {
		n := work[len(work)-1]
		work = work[:len(work)-1]
		for _, p := range pred[n] {
			if !seen[p] {
				seen[p] = true
				work = append(work, p)
			}
		}
	}
	delete(seen, exit)
	out := []string{}
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// bumpSlice is the backward slice of Bump from its `return y` (150:2) on y,
// derived by hand before BuildDDG existed:
//
//	ddg (TestDDG_LoopCarried): @entry→147:2 n; 146:2→148:3 y; 146:2→150:2 y;
//	  147:2→147:2 i; 148:3→148:3 y; 148:3→150:2 y.
//	cdg (postdom_test): 147:2→148:3; @entry→146:2, 147:2, 150:2.
//	150:2 ← 146:2, 148:3 (ddg y), @entry (cdg).
//	148:3 ← 146:2, 148:3 (ddg y), 147:2 (cdg: the loop header controls it).
//	147:2 ← @entry (ddg n — @entry defines the param n; and cdg), itself.
//	146:2 ← @entry (cdg).
//
// So every node but @exit. @entry IS included: it defines the parameter n
// that the loop condition reads, and it anchors the uncontrolled nodes.
var bumpSlice = []string{"146:2", "147:2", "148:3", "150:2", entry}

// guardSlice is the backward slice of Guard (appended to flow.go) from its
// `return r` (476:2) on r. DefUse: @entry D a, c; 470:2 r := 0 D r;
// 471:2 d := a + 1 D d U a; 472:2 if c > 0 U c; 473:3 r = a D r U a;
// 475:2 _ = d U d; 476:2 return r U r.
//
//	ddg: @entry→471:2 a; @entry→472:2 c; @entry→473:3 a; 471:2→475:2 d;
//	  470:2→476:2 r; 473:3→476:2 r.
//	cdg: 472:2→473:3; @entry→470:2, 471:2, 472:2, 475:2, 476:2 (the if has
//	  no else and rejoins at 475:2, which post-dominates it).
//	476:2 ← 470:2, 473:3 (ddg r), @entry (cdg).
//	473:3 ← @entry (ddg a), 472:2 (cdg).
//	472:2 ← @entry (ddg c, cdg).   470:2 ← @entry (cdg).
//
// 472:2 is reached only through the CDG: the DDG alone from 476:2 gives
// {476:2, 470:2, 473:3, @entry}. 471:2 and 475:2 (the d chain) are out.
// @entry is included.
var guardSlice = []string{"470:2", "472:2", "473:3", "476:2", entry}

func TestSlice_BackwardExact(t *testing.T) {
	_, cdg, ddg := ddgOf(t, "Bump")
	if got := sliceOf(cdg, ddg, "150:2"); !reflect.DeepEqual(got, bumpSlice) {
		t.Errorf("Bump slice from 150:2 = %v, want %v", got, bumpSlice)
	}

	_, cdg, ddg = ddgOf(t, "Guard")
	if got := sliceOf(cdg, ddg, "476:2"); !reflect.DeepEqual(got, guardSlice) {
		t.Errorf("Guard slice from 476:2 = %v, want %v", got, guardSlice)
	}
	// The CDG contributes: the DDG alone does not reach the if header.
	if got := sliceOf(nil, ddg, "476:2"); reflect.DeepEqual(got, guardSlice) {
		t.Errorf("Guard: DDG-only slice %v already equals the PDG slice; the gate does not exercise the CDG", got)
	}
}

// TestDDG_CorpusGate runs BuildDDG over every function and function literal
// body in testdata/dataflow and testdata/chi: no panic, the output contract
// (checkDDG: endpoints are pruned non-satellite nodes, sorted, deduped), two
// builds deep-equal, and the result independent of input order (nodes, CFG
// edges and every DefUse list reversed).
func TestDDG_CorpusGate(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(thisFile), "..", "..", "testdata")
	total, edges := 0, 0
	for _, dir := range []string{"dataflow", "chi"} {
		fset, cases := loadDiffCases(t, filepath.Join(root, dir))
		for _, c := range cases {
			total++
			nodes, keyOf := dataflow.BuildNodes(fset, c.body, gateCallKeys(fset, c.body))
			kept, es := dataflow.BuildCFG(nodes, keyOf, c.body, c.info)
			du := duMap(kept, c.info, c.fn, keyOf)
			got := dataflow.BuildDDG(kept, es, du)
			checkDDG(t, c.name, kept, got)
			edges += len(got)

			if again := dataflow.BuildDDG(kept, es, duMap(kept, c.info, c.fn, keyOf)); !reflect.DeepEqual(got, again) {
				t.Errorf("%s: two builds differ", c.name)
			}
			rn := append([]dataflow.Node(nil), kept...)
			reverse(rn)
			re := append([]dataflow.CfgEdge(nil), es...)
			reverse(re)
			rdu := map[string]dataflow.DefUse{}
			for k, v := range du {
				rdu[k] = dataflow.DefUse{Defs: reversed(v.Defs), WeakDefs: reversed(v.WeakDefs), Uses: reversed(v.Uses)}
			}
			if perm := dataflow.BuildDDG(rn, re, rdu); !reflect.DeepEqual(got, perm) {
				t.Errorf("%s: DDG depends on input order", c.name)
			}
		}
	}
	if total < 100 || edges < 1000 {
		t.Errorf("ddg gate covered only %d bodies / %d edges", total, edges)
	}
	t.Logf("ddg gate: %d bodies, %d edges", total, edges)
}

func reverse[T any](s []T) {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
}

func reversed(ps []dataflow.Path) []dataflow.Path {
	out := append([]dataflow.Path(nil), ps...)
	reverse(out)
	return out
}

// hasDDG asserts fn's DDG contains each of want (not necessarily only them).
func hasDDG(t *testing.T, fn string, want ...dataflow.DdgEdge) {
	t.Helper()
	_, _, got := ddgOf(t, fn)
	have := map[dataflow.DdgEdge]bool{}
	for _, g := range got {
		have[g] = true
	}
	for _, w := range want {
		if !have[w] {
			t.Errorf("%s: missing ddg %s→%s on %q; have %v", fn, w.Src, w.Dst, w.Var, got)
		}
	}
}

// TestDDG_SubsliceArgWritesBase (C1): a callee writing through a subslice
// (buf[:n], arr[:], dst[i:]) writes the base's elements: the call
// weak-defines base[*], which reaches a later read of the base.
func TestDDG_SubsliceArgWritesBase(t *testing.T) {
	hasDDG(t, "SubsliceRead", dd("487:2", "488:2", "buf[*]"))
	hasDDG(t, "ArraySliceRead", dd("494:2", "495:2", "arr[*]"))
	hasDDG(t, "CopyInto", dd("501:2", "502:2", "dst[*]"))
}

// TestDDG_DeferredCallReadsStateAtExit (I7): a deferred call runs at exit and
// reads through its receiver and reference-typed arguments then, so a write
// after its registration reaches it.
func TestDDG_DeferredCallReadsStateAtExit(t *testing.T) {
	hasDDG(t, "DeferFlush",
		dd("524:2", "522:8", "w.*"),  // w.buf = nil → deferred w.Flush()
		dd("525:2", "523:8", "b[*]"), // b[0] = 1 → deferred fill(b[1:])
	)
}

// TestDDG_AppendWritesDstBackingArray: append(dst[:0], x) writes x into dst's
// backing array, so it weak-defines dst[*], which reaches a later dst[0].
func TestDDG_AppendWritesDstBackingArray(t *testing.T) {
	hasDDG(t, "AppendReuse", dd("541:2", "542:2", "dst[*]"))
}
