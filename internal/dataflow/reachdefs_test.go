package dataflow_test

import (
	"go/ast"
	"go/token"
	"go/types"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"sort"
	"testing"

	"github.com/codellm-devkit/codeanalyzer-go/internal/dataflow"
)

func dd(src, dst, v string) dataflow.DdgEdge { return dataflow.DdgEdge{Src: src, Dst: dst, Var: v} }

// duMap is DefUses of the pruned set, keyed by node Key — BuildDDG's input,
// computed the way the analyzer computes it.
func duMap(nodes []dataflow.Node, info *types.Info, fn ast.Node, keyOf map[ast.Node]string) map[string]dataflow.DefUse {
	return dataflow.DefUses(nodes, info, kTest, dataflow.EntryBases(fn, info), fn, keyOf)
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
// builds deep-equal, the result independent of input order (nodes, CFG
// edges and every DefUse list reversed), and a superset of the DDG of the
// pre-D1 def/use sets (the D1 rules only add edges).
func TestDDG_CorpusGate(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(thisFile), "..", "..", "testdata")
	total, edges, oldEdges := 0, 0, 0
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

			// DefUseOf (callable facts derived per call) agrees with DefUses.
			bases := dataflow.EntryBases(c.fn, c.info)
			for _, n := range kept {
				if one := dataflow.DefUseOf(n, c.info, kTest, bases, c.fn, keyOf); !reflect.DeepEqual(one, du[n.Key]) {
					t.Errorf("%s %s: DefUseOf %+v != DefUses %+v", c.name, n.Key, one, du[n.Key])
				}
			}

			if again := dataflow.BuildDDG(kept, es, duMap(kept, c.info, c.fn, keyOf)); !reflect.DeepEqual(got, again) {
				t.Errorf("%s: two builds differ", c.name)
			}
			rn := append([]dataflow.Node(nil), kept...)
			reverse(rn)
			re := append([]dataflow.CfgEdge(nil), es...)
			reverse(re)
			rdu := map[string]dataflow.DefUse{}
			for k, v := range du {
				rdu[k] = dataflow.DefUse{Defs: reversed(v.Defs), WeakDefs: reversed(v.WeakDefs), Uses: reversed(v.Uses), Shallow: reversed(v.Shallow)}
			}
			if perm := dataflow.BuildDDG(rn, re, rdu); !reflect.DeepEqual(got, perm) {
				t.Errorf("%s: DDG depends on input order", c.name)
			}

			// The D1 rules (#22, #28) only add edges: every edge of the
			// pre-D1 def/use sets is still there.
			have := map[dataflow.DdgEdge]bool{}
			for _, g := range got {
				have[g] = true
			}
			old := dataflow.BuildDDG(kept, es, dataflow.DefUsesPreD1(kept, c.info, kTest, dataflow.EntryBases(c.fn, c.info), c.fn, keyOf))
			for _, o := range old {
				if !have[o] {
					t.Errorf("%s: D1 rules dropped pre-D1 ddg %s→%s on %q", c.name, o.Src, o.Dst, o.Var)
				}
			}
			oldEdges += len(old)
		}
	}
	if total < 100 || edges < 1000 {
		t.Errorf("ddg gate covered only %d bodies / %d edges", total, edges)
	}
	t.Logf("ddg gate: %d bodies, %d edges (%d pre-D1, all kept)", total, edges, oldEdges)
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

// TestDDG_DeferredNamedResultWriteNeverKills (R11): in DeferWrap the exit
// chain is return 558:2 → 557:8 (err = wrapErr(err)) → 556:8 (observe(err))
// → @exit. A deferred call node never strong-defines, so err = wrapErr(nil)
// (555:2) reaches the return, the rewriting closure and, through it, the
// observer at the end of the chain. The rewriting closure weak-defines its
// captured err (D1 gap 1), so its write reaches the observer too.
func TestDDG_DeferredNamedResultWriteNeverKills(t *testing.T) {
	hasDDG(t, "DeferWrap",
		dd("555:2", "558:2", "err"), // the bare return reads the named result
		dd("555:2", "557:8", "err"), // the rewriting closure reads it at exit
		dd("555:2", "556:8", "err"), // and the def survives past it
		dd("557:8", "556:8", "err"), // the rewrite's capture write (D1 gap 1)
	)
}

// TestDDG_ClosureCaptureWrites (D1 gap 1): the node hosting a function literal
// weak-defines what the literal may write of its captures, and every strong
// def of a captured-and-written variable in the callable is weak, so the
// closure's write reaches the later read.
func TestDDG_ClosureCaptureWrites(t *testing.T) {
	// func(){ x = 1 }() at 564:2 → return x.
	hasDDG(t, "CaptureWrite", dd("564:2", "565:2", "x"))
	// h := func(){ x = 1 } at 572:2; x = 2 (573:2) does not kill it.
	hasDDG(t, "CaptureThenReassign", dd("572:2", "575:2", "x"))
	// once.Do(func(){ cfg = load() }) at 588:2 → return cfg.
	hasDDG(t, "OnceDo", dd("588:2", "589:2", "cfg"))
	// f := func(){ func(){ x = y }() } at 613:2: the nested literal's write.
	hasDDG(t, "CaptureNested", dd("613:2", "620:2", "x"))
	// go func(){ x = <-ch }() at 627:5: the go node hosts the literal.
	hasDDG(t, "CaptureGo", dd("627:5", "628:2", "x"))
}

// TestDDG_DeferOperandAfterReassign (D1 gap 3): in `defer fill(b); b[0] = s;
// b = o` the element write also weak-defines the defer's pseudo path, which
// b = o does not kill, so it reaches the deferred fill (595:8).
func TestDDG_DeferOperandAfterReassign(t *testing.T) {
	hasDDG(t, "DeferReassign", dd("596:2", "595:8", "<defer>:595:8"))
}

// TestDDG_AppendKeepsElementDefs (#28): s = append(s, n) may keep s's backing
// array, so it does not kill the earlier s[0] = 5.
func TestDDG_AppendKeepsElementDefs(t *testing.T) {
	hasDDG(t, "AppendKeepsElems", dd("603:2", "605:2", "s[*]"))
}

// TestDDG_ValueStructWithSliceIsWrittenThrough (D1 gap 2): fillCfg(c) with a
// value struct c whose field items is a slice may write c.items[*]; the call
// weak-defines c.*, which reaches the later read of c.items[0].
func TestDDG_ValueStructWithSliceIsWrittenThrough(t *testing.T) {
	wantDDG(t, "FillCfg",
		dd(entry, "651:2", "c"),
		dd(entry, "652:2", "c.items[*]"),
		dd("651:2", "652:2", "c.items[*]"),
	)
	// A value receiver over a slice field (c.reset()) and value arguments
	// that may hold a reference (n nests one, a is an array of slices) are
	// written through; p (ints only) and b ([2]int) are not.
	hasDDG(t, "ValueRefs",
		dd("659:2", "662:2", "c.items[*]"),
		dd("661:2", "662:2", "n.in.items[*]"),
		dd("661:2", "662:2", "a[*][*]"),
	)
	_, _, got := ddgOf(t, "ValueRefs")
	for _, g := range got {
		if g.Src != entry && (g.Var == "p.a" || g.Var == "b[*]") {
			t.Errorf("ValueRefs: a reference-free value operand is written through: %v", g)
		}
	}
}

// flatDef is a definition of the flat reference: a node and a path.
type flatDef struct {
	Node string
	Path dataflow.Path
}

// flatReaching is a reference reaching-definitions fixpoint with the kill
// rule of BuildDDG tested against every definition of the body (no
// bucketing), on plain map sets: IN of every CFG node.
func flatReaching(nodes []dataflow.Node, cfg []dataflow.CfgEdge, du map[string]dataflow.DefUse) map[string]map[flatDef]bool {
	type def = flatDef
	isNode := map[string]bool{}
	for _, n := range nodes {
		if n.Parent == "" {
			isNode[n.Key] = true
		}
	}
	pred := map[string][]string{}
	for _, e := range cfg {
		if isNode[e.Src] && isNode[e.Dst] {
			pred[e.Dst] = append(pred[e.Dst], e.Src)
		}
	}
	var all []def
	for k := range isNode {
		d := du[k]
		for _, list := range [][]dataflow.Path{d.Defs, d.WeakDefs} {
			for _, p := range list {
				all = append(all, def{k, p})
			}
		}
	}
	killed := func(k string, d def) bool {
		shallow := map[dataflow.Path]bool{}
		for _, p := range du[k].Shallow {
			shallow[p] = true
		}
		for _, p := range du[k].Defs {
			if p == d.Path || !shallow[p] && p.Covers(d.Path) {
				return true
			}
		}
		return false
	}
	in, out := map[string]map[def]bool{}, map[string]map[def]bool{}
	for k := range isNode {
		in[k], out[k] = map[def]bool{}, map[def]bool{}
	}
	for changed := true; changed; {
		changed = false
		for k := range isNode {
			for _, p := range pred[k] {
				for d := range out[p] {
					in[k][d] = true
				}
			}
			next := map[def]bool{}
			for d := range in[k] {
				if !killed(k, d) {
					next[d] = true
				}
			}
			for _, d := range all {
				if d.Node == k {
					next[d] = true
				}
			}
			if len(next) != len(out[k]) {
				changed = true
			} else {
				for d := range next {
					if !out[k][d] {
						changed = true
						break
					}
				}
			}
			out[k] = next
		}
	}
	return in
}

// flatDDG is the reference DDG over flatReaching: each use links from each
// reaching def it interferes with. TestDDG_KillBucketsMatchFlat compares
// BuildDDG against it.
func flatDDG(nodes []dataflow.Node, cfg []dataflow.CfgEdge, du map[string]dataflow.DefUse) []dataflow.DdgEdge {
	return flatDDGAlias(nodes, cfg, du, nil)
}

// anyBase is the broadest oracle: any two resolved bases may alias. The flat
// alias gate uses it so that the matching, not the oracle, is under test.
func anyBase(a, b dataflow.Path) bool { return a.Base != nil && b.Base != nil }

// flatDDGAlias is BuildDDGAlias's reference, formulated independently: over
// flatReaching, a use u also links from a reaching def d of another base
// when mayAlias(d, u) and, dropping at most one leading step from each, d's
// remaining steps rebased onto u's base interfere with u's remaining steps
// rebased likewise (Covers either way). nil mayAlias is flatDDG.
func flatDDGAlias(nodes []dataflow.Node, cfg []dataflow.CfgEdge, du map[string]dataflow.DefUse, mayAlias func(a, b dataflow.Path) bool) []dataflow.DdgEdge {
	edges := map[dataflow.DdgEdge]bool{}
	for k, ins := range flatReaching(nodes, cfg, du) {
		for d := range ins {
			for _, u := range du[k].Uses {
				if d.Path.Covers(u) || u.Covers(d.Path) {
					edges[dataflow.DdgEdge{Src: d.Node, Dst: k, Var: u.Spell}] = true
					continue
				}
				if mayAlias == nil || d.Path.Base == nil || u.Base == d.Path.Base {
					continue
				}
				if flatWindow(d.Path, u) && mayAlias(d.Path, u) {
					edges[dataflow.DdgEdge{Src: d.Node, Dst: k, Var: u.Spell}] = true
				}
			}
		}
	}
	res := []dataflow.DdgEdge{}
	for e := range edges {
		res = append(res, e)
	}
	sort.Slice(res, func(i, j int) bool { return ddgLess(res[i], res[j]) })
	return res
}

// flatWindow is the one-step offset window on spellings: both suffixes,
// each with up to one leading step dropped, rebased onto u's base.
func flatWindow(d, u dataflow.Path) bool {
	tails := func(p dataflow.Path) []string {
		suf := p.Spell[len(dataflow.SpellBase(p.Spell)):]
		out := []string{suf}
		if steps := dataflow.PathSteps(p.Spell); len(steps) > 0 {
			out = append(out, suf[len(steps[0]):])
		}
		return out
	}
	head := dataflow.SpellBase(u.Spell)
	for _, ds := range tails(d) {
		for _, us := range tails(u) {
			a := dataflow.Path{Base: u.Base, Spell: head + ds}
			b := dataflow.Path{Base: u.Base, Spell: head + us}
			if a.Covers(b) || b.Covers(a) {
				return true
			}
		}
	}
	return false
}

// TestDDG_KillBucketsMatchFlat (#27 item 1): BuildDDG tests kills only within
// the bucket of a def's base (nil-base paths by spelling head); it equals the
// flat reference over the whole flow and chi corpus and over a hand-built
// line mixing shadowed bases, unresolved (nil-base) paths sharing a spelling
// with a real base, and pseudo paths.
func TestDDG_KillBucketsMatchFlat(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(thisFile), "..", "..", "testdata")
	total, aliased := 0, 0
	for _, dir := range []string{"dataflow", "chi"} {
		fset, cases := loadDiffCases(t, filepath.Join(root, dir))
		for _, c := range cases {
			nodes, keyOf := dataflow.BuildNodes(fset, c.body, gateCallKeys(fset, c.body))
			kept, es := dataflow.BuildCFG(nodes, keyOf, c.body, c.info)
			du := duMap(kept, c.info, c.fn, keyOf)
			got, want := dataflow.BuildDDG(kept, es, du), flatDDG(kept, es, du)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s: bucketed DDG (%d edges) != flat (%d edges)", c.name, len(got), len(want))
			}
			total += len(got)
			gotA, wantA := dataflow.BuildDDGAlias(kept, es, du, anyBase), flatDDGAlias(kept, es, du, anyBase)
			if !reflect.DeepEqual(gotA, wantA) {
				t.Errorf("%s: bucketed alias DDG (%d edges) != flat (%d edges)", c.name, len(gotA), len(wantA))
			}
			aliased += len(gotA) - len(got)
		}
	}
	if total < 1000 {
		t.Errorf("kill-bucket gate covered only %d edges", total)
	}
	if aliased < 100 {
		t.Errorf("alias gate covered only %d alias edges", aliased)
	}

	// Hand-built: x (outer), x (shadowing inner, same spelling), the
	// unresolved x and u (nil base), and a <defer>: pseudo path.
	xo := types.NewVar(token.NoPos, nil, "x", types.Typ[types.Int])
	xi := types.NewVar(token.NoPos, nil, "x", types.Typ[types.Int])
	P := func(b types.Object, s ...string) []dataflow.Path {
		var out []dataflow.Path
		for _, sp := range s {
			out = append(out, dataflow.Path{Base: b, Spell: sp})
		}
		return out
	}
	cat := func(ps ...[]dataflow.Path) []dataflow.Path {
		var out []dataflow.Path
		for _, p := range ps {
			out = append(out, p...)
		}
		return out
	}
	keys := []string{entry, "1:1", "1:2", "1:3", "1:4", "1:5", "1:6", exit}
	var nodes []dataflow.Node
	var cfg []dataflow.CfgEdge
	for i, k := range keys {
		nodes = append(nodes, dataflow.Node{Key: k})
		if i > 0 {
			cfg = append(cfg, dataflow.CfgEdge{Src: keys[i-1], Dst: k, Kind: "fallthrough"})
		}
	}
	cfg = append(cfg, dataflow.CfgEdge{Src: "1:5", Dst: "1:2", Kind: "loop_back"})
	du := map[string]dataflow.DefUse{
		entry: {Defs: cat(P(xo, "x"), P(xi, "x.f"))},
		"1:1": {Defs: P(nil, "x", "u.f"), WeakDefs: P(nil, "<defer>:9:9", "u.*")},
		"1:2": {Defs: P(xi, "x"), Uses: cat(P(xo, "x"), P(nil, "x", "u"))},
		"1:3": {Defs: P(nil, "u.f"), Uses: cat(P(xi, "x.f"), P(nil, "u.f.g"))},
		"1:4": {Defs: cat(P(xo, "x"), P(nil, "<defer>:9:9", "u")), Shallow: P(xo, "x"), WeakDefs: P(xo, "x[*]")},
		"1:5": {Uses: cat(P(xo, "x[*]"), P(nil, "x", "u", "<defer>:9:9"), P(xi, "x"))},
		"1:6": {Defs: P(nil, "u"), Uses: cat(P(xo, "x"), P(nil, "u.f", "<defer>:9:9"))},
	}
	got, want := dataflow.BuildDDG(nodes, cfg, du), flatDDG(nodes, cfg, du)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("hand-built kills:\n got %v\nwant %v", got, want)
	}
	if gotA, wantA := dataflow.BuildDDGAlias(nodes, cfg, du, anyBase), flatDDGAlias(nodes, cfg, du, anyBase); !reflect.DeepEqual(gotA, wantA) {
		t.Errorf("hand-built alias kills:\n got %v\nwant %v", gotA, wantA)
	}
	// The unresolved u (1:4) kills u.f of 1:1 and 1:3 by spelling head: only
	// 1:4's own u reaches 1:6's u.f.
	for _, g := range got {
		if g.Dst == "1:6" && g.Var == "u.f" && g.Src != "1:4" {
			t.Errorf("hand-built kills: %v survives the unresolved u = … at 1:4", g)
		}
	}
}

// TestDDGAlias_Rule pins BuildDDGAlias on a hand-built line, with p and q
// pointers of one type and v an int, under an oracle that, like the L4
// typeOracle, lets only a write through a pointer (a def with a step) alias:
//
//	@entry  defines p, q, v
//	1:1     p.f = v
//	1:2     q.f = v; q.g.h = v (strong defs through q: kill q.f…, q.g.h… only)
//	1:3     use p.f, q.f.g, q, p.g, p.h.f.k, p.k.m
//	1:4     q = nil            (a bare def: rebinds q, writes no aliased memory)
//	1:5     use p.f
//
// 1:1's p.f reaches 1:2's bare use q. At 1:3, by alias: 1:2's q.f may have
// written p.f, yet 1:1's p.f still reaches by its own base (alias never
// kills); 1:1's p.f reaches q.f.g (its suffix covers) and the bare use q.
// The one-step offset window admits p.g and p.k.m against q.f (one leading
// step dropped from each leaves [] against [] or [.m]: q.f's storage may be
// what p.k points to), and p.h.f.k against q.f (.h dropped); the oracle
// decides them (yes). p.k.m against q.g.h is outside the window ([.h]
// against [.m] at best): the oracle is never asked about that pair. At 1:5 nothing aliases: 1:4
// killed q's defs by its own base, and its bare def of q aliases nothing,
// like @entry's.
func TestDDGAlias_Rule(t *testing.T) {
	ptr := types.NewPointer(types.NewStruct(nil, nil))
	p := types.NewVar(token.NoPos, nil, "p", ptr)
	q := types.NewVar(token.NoPos, nil, "q", ptr)
	v := types.NewVar(token.NoPos, nil, "v", types.Typ[types.Int])
	P := func(b types.Object, s string) dataflow.Path { return dataflow.Path{Base: b, Spell: s} }
	keys := []string{entry, "1:1", "1:2", "1:3", "1:4", "1:5", exit}
	var nodes []dataflow.Node
	var cfg []dataflow.CfgEdge
	for i, k := range keys {
		nodes = append(nodes, dataflow.Node{Key: k})
		if i > 0 {
			cfg = append(cfg, dataflow.CfgEdge{Src: keys[i-1], Dst: k, Kind: "fallthrough"})
		}
	}
	du := map[string]dataflow.DefUse{
		entry: {Defs: []dataflow.Path{P(p, "p"), P(q, "q"), P(v, "v")}},
		"1:1": {Defs: []dataflow.Path{P(p, "p.f")}, Uses: []dataflow.Path{P(p, "p"), P(v, "v")}},
		"1:2": {Defs: []dataflow.Path{P(q, "q.f"), P(q, "q.g.h")}, Uses: []dataflow.Path{P(q, "q"), P(v, "v")}},
		"1:3": {Uses: []dataflow.Path{P(p, "p.f"), P(q, "q.f.g"), P(q, "q"), P(p, "p.g"), P(p, "p.h.f.k"), P(p, "p.k.m")}},
		"1:4": {Defs: []dataflow.Path{P(q, "q")}},
		"1:5": {Uses: []dataflow.Path{P(p, "p.f")}},
	}
	asked := map[[2]string]bool{}
	mayAlias := func(a, b dataflow.Path) bool {
		asked[[2]string{a.Spell, b.Spell}] = true
		return a.Base != v && b.Base != v && a.Base != nil && b.Base != nil && len(dataflow.PathSteps(a.Spell)) > 0
	}
	base := dataflow.BuildDDG(nodes, cfg, du)
	got := dataflow.BuildDDGAlias(nodes, cfg, du, mayAlias)
	ddg, alias := dataflow.BuildDDGSplit(nodes, cfg, du, mayAlias)
	if !reflect.DeepEqual(ddg, base) {
		t.Errorf("split ddg != BuildDDG:\n got %v\nwant %v", ddg, base)
	}
	var plain []dataflow.DdgEdge
	for _, e := range alias {
		if e.Def != "" {
			t.Errorf("%v: Def set off @entry", e)
		}
		plain = append(plain, e.DdgEdge)
	}
	want := []dataflow.DdgEdge{
		dd("1:1", "1:2", "q"), // the bare use q at 1:2 reads the object p.f is in
		dd("1:1", "1:3", "q"),
		dd("1:1", "1:3", "q.f.g"),
		dd("1:2", "1:3", "p.f"),
		dd("1:2", "1:3", "p.g"),
		dd("1:2", "1:3", "p.h.f.k"),
		dd("1:2", "1:3", "p.k.m"),
	}
	if !reflect.DeepEqual(plain, want) {
		t.Errorf("alias edges:\n got %v\nwant %v", plain, want)
	}
	if asked[[2]string{"q.g.h", "p.k.m"}] {
		t.Errorf("oracle asked about q.g.h, p.k.m outside the window")
	}
	if !asked[[2]string{"q.f", "p.k.m"}] {
		t.Errorf("oracle not asked about q.f, p.k.m inside the window")
	}
	for _, e := range base {
		if !slices.Contains(got, e) {
			t.Errorf("BuildDDGAlias lost BuildDDG edge %v", e)
		}
	}
	if len(got) != len(base)+len(alias) {
		t.Errorf("BuildDDGAlias = %d edges, want BuildDDG %d + alias %d", len(got), len(base), len(alias))
	}
	for _, e := range []dataflow.DdgEdge{dd("1:1", "1:3", "p.f"), dd("1:1", "1:5", "p.f")} {
		if !slices.Contains(base, e) {
			t.Errorf("%v: q.f = v killed p.f across bases: %v", e, base)
		}
	}
	if nilA := dataflow.BuildDDGAlias(nodes, cfg, du, nil); !reflect.DeepEqual(nilA, base) {
		t.Errorf("nil oracle: got %v, want BuildDDG %v", nilA, base)
	}
}
