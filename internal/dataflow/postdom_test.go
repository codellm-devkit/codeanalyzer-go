package dataflow_test

import (
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"github.com/codellm-devkit/codeanalyzer-go/internal/dataflow"
)

func c(src, dst string) dataflow.CdgEdge { return dataflow.CdgEdge{Src: src, Dst: dst} }

// cdgOf builds fn's CFG (Tasks 5–7), asserts BuildCDG's input precondition —
// every CFG node reaches @exit — and returns the CDG with the CFG.
func cdgOf(t *testing.T, fn string) ([]dataflow.Node, []dataflow.CfgEdge, []dataflow.CdgEdge) {
	t.Helper()
	nodes, es := cfgOf(t, fn)
	requireReachesExit(t, fn, nodes, es)
	return nodes, es, dataflow.BuildCDG(nodes, es)
}

// requireReachesExit is BuildCDG's precondition, asserted by the tests (not
// by production code): every CFG node (non-satellite) reaches @exit.
func requireReachesExit(t *testing.T, fn string, nodes []dataflow.Node, es []dataflow.CfgEdge) {
	t.Helper()
	rev := make([]dataflow.CfgEdge, len(es))
	for i, g := range es {
		rev[i] = dataflow.CfgEdge{Src: g.Dst, Dst: g.Src, Kind: g.Kind}
	}
	bwd := closure(rev)[exit]
	for _, n := range nodes {
		if n.Parent == "" && n.Key != exit && !bwd[n.Key] {
			t.Fatalf("%s: precondition: node %s cannot reach @exit", fn, n.Key)
		}
	}
}

// wantCDG asserts fn's CDG is exactly want, in (src, dst) order.
func wantCDG(t *testing.T, fn string, want ...dataflow.CdgEdge) {
	t.Helper()
	_, _, got := cdgOf(t, fn)
	have := map[dataflow.CdgEdge]bool{}
	for _, g := range got {
		have[g] = true
	}
	exp := map[dataflow.CdgEdge]bool{}
	for _, w := range want {
		exp[w] = true
		if !have[w] {
			t.Errorf("%s: missing cdg %s→%s", fn, w.Src, w.Dst)
		}
	}
	for _, g := range got {
		if !exp[g] {
			t.Errorf("%s: unexpected cdg %s→%s", fn, g.Src, g.Dst)
		}
	}
	checkCDGOrder(t, fn, got)
}

func checkCDGOrder(t *testing.T, fn string, got []dataflow.CdgEdge) {
	t.Helper()
	for i := 1; i < len(got); i++ {
		a, b := got[i-1], got[i]
		if a.Src > b.Src || a.Src == b.Src && a.Dst >= b.Dst {
			t.Errorf("%s: cdg out of order or duplicated: %v before %v", fn, a, b)
		}
	}
}

// Derivations below use the CFGs pinned in cfg_test.go. ipdom(n) is n's
// immediate post-dominator; FOW adds a→x for each CFG edge a→b with b ≠
// ipdom(a), for x on the post-dominator-tree path b … ipdom(a) (excluded).
// Anchoring: every non-synthetic CFG node with no controller gets @entry→x;
// @entry and @exit are never anchored (Python parity).

func TestCDG_IfElse(t *testing.T) {
	// ipdom: 17:3, 21:2 → @exit; 19:3 → 21:2; 16:2 → @exit; @entry → 16:2.
	// 16:2→17:3: walk 17:3 (stop @exit). 16:2→19:3: walk 19:3, 21:2.
	// The then-arm returns, so the statement after the if (21:2) runs only
	// on the false path: it depends on the if, not on @entry.
	// Anchored: 16:2.
	wantCDG(t, "IfElse",
		c(ieIf, ieThenRet),
		c(ieIf, ieElseAsg),
		c(ieIf, ieRet),
		c(entry, ieIf),
	)
}

func TestCDG_Loop(t *testing.T) {
	// ForLoop. ipdom: 30:2 → @exit; 27:2 → 30:2; 28:3 → 27:2; 26:2 → 27:2.
	// 27:2→28:3: walk 28:3, 27:2 (stop 30:2); 27:2→27:2 is a self-loop,
	// skipped. 28:3→27:2 is ipdom(28:3): nothing. The header has no other
	// controller, so it is anchored with 26:2 and 30:2.
	wantCDG(t, "ForLoop",
		c(flFor, flBody),
		c(entry, flInit),
		c(entry, flFor),
		c(entry, flRet),
	)
	// Spin: two empty-body loops, i.e. CFG self-loops 294:2→294:2 and
	// 296:2→296:2. ipdom: 294:2 → 296:2 → @exit. The only non-ipdom edges
	// are the self-loops, whose walk visits only the loop itself (skipped):
	// everything is anchored.
	wantCDG(t, "Spin",
		c(entry, "294:2"),
		c(entry, "296:2"),
	)
}

func TestCDG_Infinite(t *testing.T) {
	// for {} with the synthetic 35:2→@exit exception edge. ipdom: 35:2 →
	// @exit (its successors 36:3 and @exit meet at @exit); 36:3 → 37:3;
	// 37:3 → @exit (35:2 and 38:4 meet at @exit); 38:4 → @exit.
	// 35:2→36:3: walk 36:3, 37:3. 37:3→35:2: walk 35:2. 37:3→38:4: walk
	// 38:4. The header is controlled by the if (the only way back into it),
	// so it is not anchored: nothing is.
	wantCDG(t, "Infinite",
		c(infFor, infRecv),
		c(infFor, infIf),
		c(infIf, infFor),
		c(infIf, infRet),
	)
}

func TestCDG_DeferChain(t *testing.T) {
	// EarlyReturnDefer. Every exit (129:3, 132:2, the exception edges of
	// 127:2 and 131:2) enters D1 = 127:8, then D2 = 126:8, then @exit, so
	// the chain post-dominates every statement. ipdom: 126:8 → @exit;
	// 127:8 → 126:8; 127:2, 128:2, 129:3, 131:2, 132:2 → 127:8;
	// 126:2 → 127:2. Parallel defer_resume/exception edges are one edge.
	// 127:2→128:2 (exception twin to 127:8): walk 128:2. 128:2→129:3,
	// 128:2→131:2, 131:2→132:2: one node each. Anchored: 126:2, 127:2, both
	// deferred calls. The satellite g(a) at 127:13 is not a CFG
	// node and gets no CDG edge.
	wantCDG(t, "EarlyReturnDefer",
		c(erdReg2, erdIf),
		c(erdIf, erdRetNeg),
		c(erdIf, erdCall),
		c(erdCall, erdRet),
		c(entry, erdReg1),
		c(entry, erdD2),
		c(entry, erdReg2),
		c(entry, erdD1),
	)
}

func TestCDG_Switch(t *testing.T) {
	// Switch with fallthrough. ipdom: 64:2 → @exit; 60:3, 62:3 → 64:2;
	// 58:3, 59:2 → 60:3; 57:3 → 58:3; 56:2 → 57:3; 61:2 → 62:3; 55:2 → 64:2.
	// 55:2→56:2: walk 56:2, 57:3, 58:3, 60:3. 55:2→59:2: walk 59:2, 60:3.
	// 55:2→61:2: walk 61:2, 62:3. 60:3 (reached by case 1's fallthrough and
	// case 2, not default) depends on the header. Anchored: 54:2, 55:2,
	// 64:2.
	wantCDG(t, "Switch",
		c(swHdr, swCase1),
		c(swHdr, swAsg1),
		c(swHdr, swFall),
		c(swHdr, swCase2),
		c(swHdr, swAsg2),
		c(swHdr, swDefault),
		c(swHdr, swAsgD),
		c(entry, swInit),
		c(entry, swHdr),
		c(entry, swRet),
	)
}

func TestCDG_Select(t *testing.T) {
	// Select2. ipdom: 83:2 → 84:3; 82:2, 84:3, 85:2, 86:3 → @exit.
	// 82:2→83:2: walk 83:2, 84:3. 82:2→85:2: walk 85:2. 85:2→86:3 (the
	// send's exception edge goes to @exit, ipdom(85:2)): walk 86:3.
	// Anchored: 82:2.
	wantCDG(t, "Select2",
		c(selHdr, selRecv),
		c(selHdr, selRetX),
		c(selHdr, selSend),
		c(selSend, selRet0),
		c(entry, selHdr),
	)
}

func TestPostDominators_Infinite(t *testing.T) {
	nodes, es := cfgOf(t, "Infinite")
	got := dataflow.PostDominators(nodes, es)
	want := map[string]string{
		entry:   infFor,
		infFor:  exit,
		infRecv: infIf,
		infIf:   exit,
		infRet:  exit,
		exit:    exit, // the tree root maps to itself
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Infinite ipdom = %v, want %v", got, want)
	}
}

// TestCDG_NoExitPath: outside the precondition, a node that cannot reach
// @exit has no post-dominator and no controller but @entry; BuildCDG never
// panics. Edges with an endpoint that is not a node are ignored.
func TestCDG_NoExitPath(t *testing.T) {
	n := func(k string) dataflow.Node { return dataflow.Node{Key: k} }
	nodes := []dataflow.Node{n(entry), n("a"), n("b"), n("c"), n(exit)}
	es := []dataflow.CfgEdge{
		e(entry, "a", "fallthrough"),
		e("a", "b", "true"), // b, c cycle forever
		e("a", exit, "false"),
		e("b", "c", "fallthrough"),
		e("c", "b", "loop_back"),
		e("c", "ghost", "fallthrough"),
	}
	ipdom := dataflow.PostDominators(nodes, es)
	if want := map[string]string{entry: "a", "a": exit, exit: exit}; !reflect.DeepEqual(ipdom, want) {
		t.Errorf("ipdom = %v, want %v", ipdom, want)
	}
	got := dataflow.BuildCDG(nodes, es)
	want := []dataflow.CdgEdge{c(entry, "a"), c(entry, "b"), c(entry, "c")}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("cdg = %v, want %v", got, want)
	}

	// No @exit at all: nothing has a post-dominator; everything is anchored.
	got = dataflow.BuildCDG([]dataflow.Node{n(entry), n("a")}, []dataflow.CfgEdge{e(entry, "a", "fallthrough")})
	if want := []dataflow.CdgEdge{c(entry, "a")}; !reflect.DeepEqual(got, want) {
		t.Errorf("no-exit cdg = %v, want %v", got, want)
	}
}

func TestCDG_EmptyBody(t *testing.T) {
	if got := dataflow.BuildCDG(nil, nil); got == nil || len(got) != 0 {
		t.Errorf("BuildCDG(nil, nil) = %#v, want empty non-nil", got)
	}
	// Empty body: @entry → @exit only. Neither synthetic node is anchored,
	// so the CDG is empty: no @entry→@exit, no @entry→@entry.
	got := dataflow.BuildCDG(
		[]dataflow.Node{{Key: entry, Kind: dataflow.KindEntry}, {Key: exit, Kind: dataflow.KindExit}},
		[]dataflow.CfgEdge{e(entry, exit, "return")},
	)
	if got == nil || len(got) != 0 {
		t.Errorf("empty-body cdg = %#v, want empty non-nil", got)
	}
}

// TestCDG_Gate runs BuildCDG over every function and function literal body in
// testdata/dataflow and testdata/chi. Gates: two builds deep-equal; the CDG is
// independent of edge kinds (R10: every kind rewritten to one, plus a parallel
// twin of every edge with another kind) and of input edge order; every
// non-synthetic CFG node has an incoming CDG edge; every endpoint is a CFG
// node key (never a satellite); no self-loop, no edge into @entry or @exit;
// sorted and deduped; every node has a post-dominator chain ending at @exit.
func TestCDG_Gate(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(thisFile), "..", "..", "testdata")
	total, edgesN := 0, 0
	for _, dir := range []string{"dataflow", "chi"} {
		fset, cases := loadDiffCases(t, filepath.Join(root, dir))
		for _, cs := range cases {
			total++
			nodes, keyOf := dataflow.BuildNodes(fset, cs.body, gateCallKeys(fset, cs.body))
			kept, es := dataflow.BuildCFG(nodes, keyOf, cs.body, cs.info)
			requireReachesExit(t, cs.name, kept, es)
			got := dataflow.BuildCDG(kept, es)
			edgesN += len(got)

			if again := dataflow.BuildCDG(kept, es); !reflect.DeepEqual(got, again) {
				t.Errorf("%s: two builds differ", cs.name)
			}
			blind := make([]dataflow.CfgEdge, 0, 2*len(es))
			for i := len(es) - 1; i >= 0; i-- {
				blind = append(blind, e(es[i].Src, es[i].Dst, "k"), e(es[i].Src, es[i].Dst, "k2"))
			}
			if other := dataflow.BuildCDG(kept, blind); !reflect.DeepEqual(got, other) {
				t.Errorf("%s: CDG depends on edge kinds or order:\n got %v\nwant %v", cs.name, other, got)
			}
			gateCDG(t, cs.name, kept, es, got)
		}
	}
	if total < 100 || edgesN == 0 {
		t.Errorf("cdg gate covered only %d bodies / %d edges", total, edgesN)
	}
	t.Logf("cdg gate: %d bodies, %d cdg edges", total, edgesN)
}

func gateCDG(t *testing.T, name string, nodes []dataflow.Node, es []dataflow.CfgEdge, cdg []dataflow.CdgEdge) {
	t.Helper()
	cfgNode := map[string]bool{}
	for _, n := range nodes {
		if n.Parent == "" {
			cfgNode[n.Key] = true
		}
	}
	in := map[string]bool{}
	for _, g := range cdg {
		if !cfgNode[g.Src] || !cfgNode[g.Dst] {
			t.Errorf("%s: cdg %v endpoint is not a CFG node", name, g)
		}
		if g.Src == g.Dst {
			t.Errorf("%s: cdg self-loop %v", name, g)
		}
		if g.Dst == entry || g.Dst == exit {
			t.Errorf("%s: cdg edge into a synthetic node %v", name, g)
		}
		in[g.Dst] = true
	}
	for k := range cfgNode {
		if k != entry && k != exit && !in[k] {
			t.Errorf("%s: node %s has no incoming cdg edge", name, k)
		}
	}
	checkCDGOrder(t, name, cdg)

	ipdom := dataflow.PostDominators(nodes, es)
	for k := range cfgNode {
		seen := map[string]bool{}
		for cur := k; cur != exit; cur = ipdom[cur] {
			if _, ok := ipdom[cur]; !ok || seen[cur] {
				t.Errorf("%s: post-dominator chain of %s broken at %s", name, k, cur)
				break
			}
			seen[cur] = true
		}
	}
}
