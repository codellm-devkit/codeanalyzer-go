package dataflow_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/tools/go/cfg"
	"golang.org/x/tools/go/packages"

	"github.com/codellm-devkit/codeanalyzer-go/internal/dataflow"
	v2 "github.com/codellm-devkit/codeanalyzer-go/internal/schema/v2"
)

// Statement keys in testdata/dataflow/flow/flow.go (line:col of each node's
// anchor). Synthetic endpoints are dataflow.EntryKey / dataflow.ExitKey.
const (
	entry = dataflow.EntryKey
	exit  = dataflow.ExitKey

	// IfElse
	ieIf, ieThenRet, ieElseAsg, ieRet = "16:2", "17:3", "19:3", "21:2"
	// ForLoop
	flInit, flFor, flBody, flRet = "26:2", "27:2", "28:3", "30:2"
	// Infinite
	infFor, infRecv, infIf, infRet = "35:2", "36:3", "37:3", "38:4"
	// Range
	rgInit, rgFor, rgBody, rgRet = "45:2", "46:2", "47:3", "49:2"
	// Switch
	swInit, swHdr, swCase1, swAsg1, swFall, swCase2, swAsg2, swDefault, swAsgD, swRet = "54:2", "55:2", "56:2", "57:3", "58:3", "59:2", "60:3", "61:2", "62:3", "64:2"
	// TypeSwitch
	tsHdr, tsInt, tsUse, tsRetInt, tsStr, tsRetStr, tsDefault, tsRetD = "69:2", "70:2", "71:3", "72:3", "73:2", "74:3", "75:2", "76:3"
	// Select2
	selHdr, selRecv, selRetX, selSend, selRet0 = "82:2", "83:2", "84:3", "85:2", "86:3"
	// Labeled
	lbInit, lbOuter, lbInner, lbIfNeg, lbCont, lbIfZero, lbBreak, lbAdd, lbRet = "92:2", "94:2", "95:3", "96:4", "97:5", "99:4", "100:5", "102:4", "105:2"
	// GotoBack
	gbInit, gbInc, gbIf, gbGoto, gbRet = "110:2", "112:2", "113:2", "114:3", "116:2"
	// EmptySelect
	esSel = "121:2"
	// SwitchNoDefault
	sndHdr, sndCase1, sndAsg, sndFall, sndCase2, sndRet = "239:2", "240:2", "241:3", "242:3", "243:2", "245:2"
	// BreakContinue
	bcFor, bcDec, bcIf5, bcCont, bcIf2, bcBreak, bcRet = "250:2", "251:3", "252:3", "253:4", "255:3", "256:4", "259:2"
	// GotoBlock
	gkIf, gkGotoM, gkDec, gkInc, gkIf5, gkGotoE, gkAsg, gkRet = "265:2", "266:3", "268:2", "271:3", "273:2", "274:3", "276:2", "280:2"
	// GotoEnd
	geIf, geGoto, geInc, geEmpty = "285:2", "286:3", "288:2", "290:1"
	// EarlyReturnDefer: d1 is the last defer in source (first in the chain).
	erdReg1, erdReg2, erdIf, erdRetNeg, erdCall, erdRet, erdD1, erdD2 = "126:2", "127:2", "128:2", "129:3", "131:2", "132:2", "127:8", "126:8"
	// Panics
	pIf, pPanic, pIndex, pRet = "137:2", "138:3", "140:2", "141:2"
)

// edges runs Tasks 5–7 over fixture function fn with its real L1 call keys
// and checks the output contract.
func edges(t *testing.T, fn string) []dataflow.CfgEdge {
	t.Helper()
	_, es := cfgOf(t, fn)
	return es
}

// cfgOf is edges, also returning the pruned node set.
func cfgOf(t *testing.T, fn string) ([]dataflow.Node, []dataflow.CfgEdge) {
	t.Helper()
	fx := loadFlow(t)
	body := fx.decl(t, fn).Body
	nodes, keyOf := dataflow.BuildNodes(fx.fset, body, fx.callKeys(t, fn))
	kept, es := dataflow.BuildCFG(nodes, keyOf, body, fx.info)
	checkContract(t, fn, kept, es)
	return kept, es
}

// checkContract asserts the output contract: every endpoint is a returned
// node and never a satellite call node; a deferred call node is linked only
// by exit-chain edges (it is entered by an exit edge, left by defer_resume /
// exception); the edges are sorted by (src, dst, kind) with no duplicates.
func checkContract(t *testing.T, fn string, nodes []dataflow.Node, es []dataflow.CfgEdge) {
	t.Helper()
	byKey := map[string]dataflow.Node{}
	for _, n := range nodes {
		byKey[n.Key] = n
	}
	for _, g := range es {
		for _, k := range []string{g.Src, g.Dst} {
			n, ok := byKey[k]
			if !ok {
				t.Errorf("%s: edge %v endpoint %s is not a node", fn, g, k)
			} else if n.Parent != "" {
				t.Errorf("%s: edge %v links satellite call %s", fn, g, k)
			}
		}
		if byKey[g.Src].Deferred && g.Kind != "defer_resume" && g.Kind != "exception" {
			t.Errorf("%s: deferred call leaves by %v", fn, g)
		}
		if byKey[g.Dst].Deferred && g.Kind != "defer_resume" && g.Kind != "exception" && g.Kind != "return" {
			t.Errorf("%s: deferred call entered by %v", fn, g)
		}
	}
	for i := 1; i < len(es); i++ {
		a, b := es[i-1], es[i]
		if a == b {
			t.Errorf("%s: duplicate edge %v", fn, b)
		}
		if a.Src > b.Src || a.Src == b.Src && (a.Dst > b.Dst || a.Dst == b.Dst && a.Kind > b.Kind) {
			t.Errorf("%s: edges out of order: %v before %v", fn, a, b)
		}
	}
}

func e(src, dst, kind string) dataflow.CfgEdge {
	return dataflow.CfgEdge{Src: src, Dst: dst, Kind: kind}
}

// wantEdges asserts the CFG of fn is exactly want (order-insensitive).
func wantEdges(t *testing.T, fn string, want ...dataflow.CfgEdge) []dataflow.CfgEdge {
	t.Helper()
	got := edges(t, fn)
	have := map[dataflow.CfgEdge]bool{}
	for _, g := range got {
		have[g] = true
	}
	exp := map[dataflow.CfgEdge]bool{}
	for _, w := range want {
		exp[w] = true
		if !have[w] {
			t.Errorf("%s: missing edge %s→%s %s", fn, w.Src, w.Dst, w.Kind)
		}
	}
	for _, g := range got {
		if !exp[g] {
			t.Errorf("%s: unexpected edge %s→%s %s", fn, g.Src, g.Dst, g.Kind)
		}
	}
	return got
}

func TestCFG_IfElse(t *testing.T) {
	// The then-arm is a return, so it leaves to @exit instead of rejoining.
	wantEdges(t, "IfElse",
		e(entry, ieIf, "fallthrough"),
		e(ieIf, ieThenRet, "true"),
		e(ieIf, ieElseAsg, "false"),
		e(ieThenRet, exit, "return"),
		e(ieElseAsg, ieRet, "fallthrough"),
		e(ieRet, exit, "return"),
	)
}

func TestCFG_ForLoop(t *testing.T) {
	// 3-clause for: init and post fold into the header.
	wantEdges(t, "ForLoop",
		e(entry, flInit, "fallthrough"),
		e(flInit, flFor, "fallthrough"),
		e(flFor, flBody, "true"),
		e(flFor, flRet, "false"),
		e(flBody, flFor, "loop_back"),
		e(flRet, exit, "return"),
	)
	// for range: same shape.
	wantEdges(t, "Range",
		e(entry, rgInit, "fallthrough"),
		e(rgInit, rgFor, "fallthrough"),
		e(rgFor, rgBody, "true"),
		e(rgFor, rgRet, "false"),
		e(rgBody, rgFor, "loop_back"),
		e(rgRet, exit, "return"),
	)
	// cond-only for with unlabeled continue / break. An else-less if at the
	// body's tail keeps its `false` kind into the header.
	wantEdges(t, "BreakContinue",
		e(entry, bcFor, "fallthrough"),
		e(bcFor, bcDec, "true"),
		e(bcFor, bcRet, "false"),
		e(bcDec, bcIf5, "fallthrough"),
		e(bcIf5, bcCont, "true"),
		e(bcIf5, bcIf2, "false"),
		e(bcCont, bcFor, "continue"),
		e(bcIf2, bcBreak, "true"),
		e(bcIf2, bcFor, "false"),
		e(bcBreak, bcRet, "break"),
		e(bcRet, exit, "return"),
	)
}

func TestCFG_Switch(t *testing.T) {
	// With a default: no header→after edge. `fallthrough` enters the next
	// clause's first body statement, not its head.
	wantEdges(t, "Switch",
		e(entry, swInit, "fallthrough"),
		e(swInit, swHdr, "fallthrough"),
		e(swHdr, swCase1, "switch_case"),
		e(swHdr, swCase2, "switch_case"),
		e(swHdr, swDefault, "switch_case"),
		e(swCase1, swAsg1, "fallthrough"),
		e(swAsg1, swFall, "fallthrough"),
		e(swFall, swAsg2, "fallthrough"),
		e(swCase2, swAsg2, "fallthrough"),
		e(swAsg2, swRet, "fallthrough"),
		e(swDefault, swAsgD, "fallthrough"),
		e(swAsgD, swRet, "fallthrough"),
		e(swRet, exit, "return"),
	)
	// No default: one extra switch_case header→after. `fallthrough` into an
	// empty clause goes to that clause's continuation (after the switch).
	wantEdges(t, "SwitchNoDefault",
		e(entry, sndHdr, "fallthrough"),
		e(sndHdr, sndCase1, "switch_case"),
		e(sndHdr, sndCase2, "switch_case"),
		e(sndHdr, sndRet, "switch_case"),
		e(sndCase1, sndAsg, "fallthrough"),
		e(sndAsg, sndFall, "fallthrough"),
		e(sndFall, sndRet, "fallthrough"),
		e(sndCase2, sndRet, "fallthrough"),
		e(sndRet, exit, "return"),
	)
	// Type switch: the guard folds into the header; every clause returns.
	wantEdges(t, "TypeSwitch",
		e(entry, tsHdr, "fallthrough"),
		e(tsHdr, tsInt, "switch_case"),
		e(tsHdr, tsStr, "switch_case"),
		e(tsHdr, tsDefault, "switch_case"),
		e(tsInt, tsUse, "fallthrough"),
		e(tsUse, tsRetInt, "fallthrough"),
		e(tsRetInt, exit, "return"),
		e(tsStr, tsRetStr, "fallthrough"),
		e(tsRetStr, exit, "return"),
		e(tsDefault, tsRetD, "fallthrough"),
		e(tsRetD, exit, "return"),
	)
}

func TestCFG_Select(t *testing.T) {
	// select blocks: header→each comm clause, never header→after (here @exit).
	wantEdges(t, "Select2",
		e(entry, selHdr, "fallthrough"),
		e(selHdr, selRecv, "switch_case"),
		e(selHdr, selSend, "switch_case"),
		e(selRecv, selRetX, "fallthrough"),
		e(selRetX, exit, "return"),
		e(selSend, selRet0, "fallthrough"),
		e(selSend, exit, "exception"), // a send on a closed channel panics
		e(selRet0, exit, "return"),
	)
	// select {} blocks forever: only the synthetic exception edge leaves it.
	wantEdges(t, "EmptySelect",
		e(entry, esSel, "fallthrough"),
		e(esSel, exit, "exception"),
	)
}

func TestCFG_LabeledBreakContinue(t *testing.T) {
	// continue outer → outer header; break outer → after the outer loop. The
	// inner loop's false edge leaves to the outer header (its continuation).
	wantEdges(t, "Labeled",
		e(entry, lbInit, "fallthrough"),
		e(lbInit, lbOuter, "fallthrough"),
		e(lbOuter, lbInner, "true"),
		e(lbOuter, lbRet, "false"),
		e(lbInner, lbIfNeg, "true"),
		e(lbInner, lbOuter, "false"),
		e(lbIfNeg, lbCont, "true"),
		e(lbIfNeg, lbIfZero, "false"),
		e(lbCont, lbOuter, "continue"),
		e(lbIfZero, lbBreak, "true"),
		e(lbIfZero, lbAdd, "false"),
		e(lbBreak, lbRet, "break"),
		e(lbAdd, lbInner, "loop_back"),
		e(lbRet, exit, "return"),
	)
	// Backward goto to a labeled statement: kind fallthrough.
	wantEdges(t, "GotoBack",
		e(entry, gbInit, "fallthrough"),
		e(gbInit, gbInc, "fallthrough"),
		e(gbInc, gbIf, "fallthrough"),
		e(gbIf, gbGoto, "true"),
		e(gbIf, gbRet, "false"),
		e(gbGoto, gbInc, "fallthrough"),
		e(gbRet, exit, "return"),
	)
}

// TestCFG_GotoLabeledBlock locks ruling R6: a label on a non-node statement
// resolves to the first node inside it, or, when it holds none, to the
// statement's continuation.
func TestCFG_GotoLabeledBlock(t *testing.T) {
	// goto M → `M: { a++ }` enters a++; goto E → `E: {}` reaches the return.
	wantEdges(t, "GotoBlock",
		e(entry, gkIf, "fallthrough"),
		e(gkIf, gkGotoM, "true"),
		e(gkIf, gkDec, "false"),
		e(gkGotoM, gkInc, "fallthrough"),
		e(gkDec, gkInc, "fallthrough"),
		e(gkInc, gkIf5, "fallthrough"),
		e(gkIf5, gkGotoE, "true"),
		e(gkIf5, gkAsg, "false"),
		e(gkGotoE, gkRet, "fallthrough"),
		e(gkAsg, gkRet, "fallthrough"),
		e(gkRet, exit, "return"),
	)
	// `L: }` labels the implicit empty statement: goto L continues past the
	// end of the body to @exit — and every non-exception edge into @exit is
	// kind return. BuildNodes still emits a zero-width node for it (290:1);
	// the CFG leaves it unlinked and unreachable, so pruning drops it from the
	// returned node set.
	got := wantEdges(t, "GotoEnd",
		e(entry, geIf, "fallthrough"),
		e(geIf, geGoto, "true"),
		e(geIf, geInc, "false"),
		e(geGoto, exit, "return"),
		e(geInc, exit, "return"),
	)
	for _, g := range got {
		if g.Src == geEmpty || g.Dst == geEmpty {
			t.Errorf("GotoEnd: implicit empty statement is linked: %v", g)
		}
	}
	nodes, _ := cfgOf(t, "GotoEnd")
	if hasNode(nodes, geEmpty) {
		t.Errorf("GotoEnd: implicit empty statement %s not pruned", geEmpty)
	}
}

func hasNode(nodes []dataflow.Node, key string) bool {
	for _, n := range nodes {
		if n.Key == key {
			return true
		}
	}
	return false
}

func TestCFG_Infinite(t *testing.T) {
	got := wantEdges(t, "Infinite",
		e(entry, infFor, "fallthrough"),
		e(infFor, infRecv, "fallthrough"),
		e(infFor, exit, "exception"), // synthetic: restores post-dominance
		e(infRecv, infIf, "fallthrough"),
		e(infIf, infRet, "true"),
		e(infIf, infFor, "false"),
		e(infRet, exit, "return"),
	)
	for _, g := range got {
		if g.Src == infFor && g.Kind == "false" {
			t.Errorf("Infinite: for {} has a false edge %v", g)
		}
	}
}

// TestCFG_EmptyLoopBody: a loop body with no node is a loop_back self-edge.
func TestCFG_EmptyLoopBody(t *testing.T) {
	wantEdges(t, "Spin",
		e(entry, "294:2", "fallthrough"),
		e("294:2", "294:2", "loop_back"),
		e("294:2", "296:2", "false"),
		e("296:2", "296:2", "loop_back"),
		e("296:2", exit, "exception"),
	)
}

// TestCFG_DeferChainOrderAndKinds: deferred calls run only in the exit chain,
// last-registered first. Edges into the chain keep their kind (return /
// exception); chain-internal edges and the tail → @exit are defer_resume, and
// a deferred call that can panic also passes an exception edge down the chain.
func TestCFG_DeferChainOrderAndKinds(t *testing.T) {
	wantEdges(t, "EarlyReturnDefer",
		e(entry, erdReg1, "fallthrough"),
		e(erdReg1, erdReg2, "fallthrough"),
		e(erdReg2, erdIf, "fallthrough"),
		e(erdReg2, erdD1, "exception"), // evaluating g(a) at registration
		e(erdIf, erdRetNeg, "true"),
		e(erdIf, erdCall, "false"),
		e(erdRetNeg, erdD1, "return"),
		e(erdCall, erdRet, "fallthrough"),
		e(erdCall, erdD1, "exception"),
		e(erdRet, erdD1, "return"),
		e(erdD1, erdD2, "defer_resume"),
		e(erdD1, erdD2, "exception"),
		e(erdD2, exit, "defer_resume"),
		e(erdD2, exit, "exception"),
	)
}

// TestCFG_PanicOnlyException: a panic(...) statement leaves only by its
// exception edge; nothing falls through it.
func TestCFG_PanicOnlyException(t *testing.T) {
	got := wantEdges(t, "Panics",
		e(entry, pIf, "fallthrough"),
		e(pIf, pPanic, "true"),
		e(pIf, pIndex, "false"),
		e(pPanic, exit, "exception"),
		e(pIndex, pRet, "fallthrough"),
		e(pIndex, exit, "exception"),
		e(pRet, exit, "return"),
	)
	if out := outEdges(got, pPanic); len(out) != 1 || out[0].Kind != "exception" {
		t.Errorf("Panics: panic out-edges = %v, want one exception", out)
	}
	// A body ending in panic falls off nowhere.
	wantEdges(t, "PanicLast",
		e(entry, "412:2", "fallthrough"),
		e("412:2", "413:2", "fallthrough"),
		e("412:2", exit, "exception"),
		e("413:2", exit, "exception"),
	)
	// A local named panic is an ordinary call: it falls through too.
	wantEdges(t, "ShadowPanic",
		e(entry, "418:2", "fallthrough"),
		e("418:2", "419:2", "fallthrough"),
		e("419:2", "420:2", "fallthrough"),
		e("419:2", exit, "exception"),
		e("420:2", exit, "return"),
	)
}

func outEdges(es []dataflow.CfgEdge, src string) []dataflow.CfgEdge {
	var out []dataflow.CfgEdge
	for _, g := range es {
		if g.Src == src {
			out = append(out, g)
		}
	}
	return out
}

// TestCFG_IndexCanPanic: `v := xs[i]` both falls through and may panic.
func TestCFG_IndexCanPanic(t *testing.T) {
	got := edges(t, "Panics")
	out := outEdges(got, pIndex)
	if len(out) != 2 || out[0] != e(pIndex, pRet, "fallthrough") || out[1] != e(pIndex, exit, "exception") {
		t.Errorf("Panics: %s out-edges = %v, want exception + fallthrough", pIndex, out)
	}
}

// TestCFG_UnreachableAfterReturnPruned: code after a return is dropped from
// both the edges and the returned node set, and a defer that is never
// registered stays out of the exit chain. Satellite calls of a kept statement
// are kept.
func TestCFG_UnreachableAfterReturnPruned(t *testing.T) {
	wantEdges(t, "Unreachable",
		e(entry, "189:2", "fallthrough"),
		e("189:2", exit, "return"),
	)
	nodes, _ := cfgOf(t, "Unreachable")
	if keys := nodeKeys(nodes); strings.Join(keys, " ") != entry+" 189:2 "+exit {
		t.Errorf("Unreachable: nodes = %v, want [@entry 189:2 @exit]", keys)
	}
	wantEdges(t, "DeadDefer",
		e(entry, "425:2", "fallthrough"),
		e("425:2", exit, "return"),
	)
	nodes, _ = cfgOf(t, "DeadDefer")
	if keys := nodeKeys(nodes); strings.Join(keys, " ") != entry+" 425:2 "+exit {
		t.Errorf("DeadDefer: nodes = %v, want [@entry 425:2 @exit]", keys)
	}
	nodes, _ = cfgOf(t, "Nested")
	if keys := nodeKeys(nodes); strings.Join(keys, " ") != entry+" 196:2 196:7 196:9 197:2 "+exit {
		t.Errorf("Nested: nodes = %v, want the satellites kept", keys)
	}
}

func nodeKeys(nodes []dataflow.Node) []string {
	keys := make([]string, len(nodes))
	for i, n := range nodes {
		keys[i] = n.Key
	}
	return keys
}

// TestCFG_GotoBackwardAndEmptySelect: a node that cannot reach @exit gets one
// synthetic exception edge to the exit target (the chain head under a defer)
// from the header of its endless loop — the goto target, found structurally.
// select {} keeps Task 6's synthetic edge, now routed through the chain.
func TestCFG_GotoBackwardAndEmptySelect(t *testing.T) {
	wantEdges(t, "GotoForever",
		e(entry, "398:2", "fallthrough"),
		e("398:2", "400:2", "fallthrough"),
		e("400:2", "401:2", "fallthrough"),
		e("400:2", "398:8", "exception"), // repair: loop header → chain
		e("401:2", "400:2", "fallthrough"),
		e("398:8", exit, "defer_resume"),
		e("398:8", exit, "exception"),
	)
	wantEdges(t, "SelectForever",
		e(entry, "406:2", "fallthrough"),
		e("406:2", "407:2", "fallthrough"),
		e("407:2", "406:8", "exception"),
		e("406:8", exit, "defer_resume"),
		e("406:8", exit, "exception"),
	)
	wantEdges(t, "EmptySelect",
		e(entry, esSel, "fallthrough"),
		e(esSel, exit, "exception"),
	)
}

// TestCFG_ConditionalAndLoopDefers: a conditional or in-loop defer is one
// chain member, assumed registered on every path (spec §4.3.1).
func TestCFG_ConditionalAndLoopDefers(t *testing.T) {
	wantEdges(t, "CondDefer",
		e(entry, "431:2", "fallthrough"),
		e("431:2", "432:3", "true"),
		e("431:2", "434:2", "false"),
		e("432:3", "434:2", "fallthrough"),
		e("434:2", "432:9", "return"),
		e("432:9", exit, "defer_resume"),
		e("432:9", exit, "exception"),
	)
	// The loop's false edge into @exit (normalized to return) enters the chain.
	wantEdges(t, "LoopDefer",
		e(entry, "439:2", "fallthrough"),
		e("439:2", "440:3", "true"),
		e("439:2", "440:9", "return"),
		e("440:3", "439:2", "loop_back"),
		e("440:9", exit, "defer_resume"),
		e("440:9", exit, "exception"),
	)
}

// TestCFG_PanicInNestedBlock: a panic in a block nested in an if in a loop
// leaves only by its exception edge, never back to the loop.
func TestCFG_PanicInNestedBlock(t *testing.T) {
	got := wantEdges(t, "LoopPanic",
		e(entry, "446:2", "fallthrough"),
		e("446:2", "447:2", "fallthrough"),
		e("447:2", "448:3", "true"),
		e("447:2", "455:2", "false"),
		e("448:3", "450:5", "true"),
		e("448:3", "453:3", "false"),
		e("450:5", exit, "exception"),
		e("453:3", "447:2", "loop_back"),
		e("455:2", exit, "return"),
	)
	if out := outEdges(got, "450:5"); len(out) != 1 {
		t.Errorf("LoopPanic: panic out-edges = %v, want one exception", out)
	}
}

// TestCFG_AfterPanicPruned: everything after a panic is pruned, and a defer
// registered after it stays out of the chain.
func TestCFG_AfterPanicPruned(t *testing.T) {
	wantEdges(t, "AfterPanic",
		e(entry, "460:2", "fallthrough"),
		e("460:2", "461:2", "fallthrough"),
		e("461:2", "460:8", "exception"),
		e("460:8", exit, "defer_resume"),
		e("460:8", exit, "exception"),
	)
	nodes, _ := cfgOf(t, "AfterPanic")
	if keys := nodeKeys(nodes); strings.Join(keys, " ") != entry+" 460:2 460:8 461:2 "+exit {
		t.Errorf("AfterPanic: nodes = %v, want [@entry 460:2 460:8 461:2 @exit]", keys)
	}
}

// ---- R9: regression locks for Task 6 constructs ----

func TestCFG_LabeledSwitchBreak(t *testing.T) {
	// break Sw from a for nested in a clause leaves the switch.
	wantEdges(t, "LabeledSwitchBreak",
		e(entry, "303:2", "fallthrough"),
		e("303:2", "304:2", "switch_case"),
		e("303:2", "313:2", "switch_case"),
		e("304:2", "305:3", "fallthrough"),
		e("305:3", "306:4", "true"),
		e("305:3", "311:3", "false"),
		e("306:4", "307:5", "true"),
		e("306:4", "309:4", "false"),
		e("307:5", "313:2", "break"),
		e("309:4", "305:3", "loop_back"),
		e("311:3", "313:2", "fallthrough"),
		e("313:2", exit, "return"),
	)
}

func TestCFG_LabeledSelectBreak(t *testing.T) {
	wantEdges(t, "LabeledSelectBreak",
		e(entry, "319:2", "fallthrough"),
		e("319:2", "320:2", "switch_case"),
		e("320:2", "321:3", "fallthrough"),
		e("321:3", "322:4", "true"),
		e("321:3", "324:3", "false"),
		e("322:4", "326:2", "break"),
		e("324:3", "326:2", "fallthrough"),
		e("326:2", exit, "return"),
	)
}

func TestCFG_ContinueInSwitch(t *testing.T) {
	// continue inside a switch targets the enclosing for header.
	wantEdges(t, "ContinueInSwitch",
		e(entry, "331:2", "fallthrough"),
		e("331:2", "332:3", "true"),
		e("331:2", "339:2", "false"),
		e("332:3", "333:3", "fallthrough"),
		e("333:3", "334:3", "switch_case"),
		e("333:3", "337:3", "switch_case"),
		e("334:3", "335:4", "fallthrough"),
		e("335:4", "331:2", "continue"),
		e("337:3", "331:2", "loop_back"),
		e("339:2", exit, "return"),
	)
}

func TestCFG_BreakInSelect(t *testing.T) {
	// break inside a select leaves the select, not the for.
	wantEdges(t, "BreakInSelect",
		e(entry, "344:2", "fallthrough"),
		e("344:2", "345:3", "true"),
		e("344:2", "354:2", "false"),
		e("345:3", "346:3", "switch_case"),
		e("346:3", "347:4", "fallthrough"),
		e("347:4", "348:5", "true"),
		e("347:4", "350:4", "false"),
		e("348:5", "352:3", "break"),
		e("350:4", "352:3", "fallthrough"),
		e("352:3", "344:2", "loop_back"),
		e("354:2", exit, "return"),
	)
}

func TestCFG_DefaultMiddle(t *testing.T) {
	// default in the middle: still no header → after edge; fallthrough into
	// and out of the default enters the next clause's body.
	wantEdges(t, "DefaultMiddle",
		e(entry, "359:2", "fallthrough"),
		e("359:2", "360:2", "fallthrough"),
		e("360:2", "361:2", "switch_case"),
		e("360:2", "364:2", "switch_case"),
		e("360:2", "367:2", "switch_case"),
		e("361:2", "362:3", "fallthrough"),
		e("362:3", "363:3", "fallthrough"),
		e("363:3", "365:3", "fallthrough"),
		e("364:2", "365:3", "fallthrough"),
		e("365:3", "366:3", "fallthrough"),
		e("366:3", "368:3", "fallthrough"),
		e("367:2", "368:3", "fallthrough"),
		e("368:3", "370:2", "fallthrough"),
		e("370:2", exit, "return"),
	)
}

func TestCFG_TypeSwitchNil(t *testing.T) {
	// case nil: is an ordinary clause; no default, so header → after too.
	wantEdges(t, "TypeSwitchNil",
		e(entry, "375:2", "fallthrough"),
		e("375:2", "376:2", "switch_case"),
		e("375:2", "378:2", "switch_case"),
		e("375:2", "381:2", "switch_case"),
		e("376:2", "377:3", "fallthrough"),
		e("377:3", exit, "return"),
		e("378:2", "379:3", "fallthrough"),
		e("379:3", exit, "return"),
		e("381:2", exit, "return"),
	)
}

func TestCFG_GotoInLoop(t *testing.T) {
	// backward goto inside a loop body; the else-less if at the body's tail
	// keeps its false kind into the header.
	wantEdges(t, "GotoInLoop",
		e(entry, "386:2", "fallthrough"),
		e("386:2", "388:3", "true"),
		e("386:2", "393:2", "false"),
		e("388:3", "389:3", "fallthrough"),
		e("389:3", "390:4", "true"),
		e("389:3", "386:2", "false"),
		e("390:4", "388:3", "fallthrough"),
		e("393:2", exit, "return"),
	)
}

// ---- CFG gates (spec §4.3) ----

// TestCFG_Gate runs the full CFG over every function and function literal
// body in testdata/dataflow and testdata/chi, with call keys built in L1
// recording order, and checks the gates: exactly one @entry and @exit; every
// CFG node reachable from @entry and reaching @exit; every satellite's parent
// kept; every non-synthetic node has a valid Pos; edges sorted and deduped;
// two builds deep-equal.
func TestCFG_Gate(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(thisFile), "..", "..", "testdata")
	total, deferred := 0, 0
	for _, dir := range []string{"dataflow", "chi"} {
		fset, cases := loadDiffCases(t, filepath.Join(root, dir))
		for _, c := range cases {
			total++
			calls := gateCallKeys(fset, c.body)
			nodes, keyOf := dataflow.BuildNodes(fset, c.body, calls)
			kept, es := dataflow.BuildCFG(nodes, keyOf, c.body, c.info)
			checkContract(t, c.name, kept, es)
			deferred += gateOne(t, c.name, kept, es)

			nodes2, keyOf2 := dataflow.BuildNodes(fset, c.body, calls)
			kept2, es2 := dataflow.BuildCFG(nodes2, keyOf2, c.body, c.info)
			if !reflect.DeepEqual(kept, kept2) || !reflect.DeepEqual(es, es2) {
				t.Errorf("%s: two builds differ", c.name)
			}
		}
	}
	if total < 100 || deferred == 0 {
		t.Errorf("gate covered only %d bodies / %d deferred calls", total, deferred)
	}
	t.Logf("gate: %d bodies, %d deferred calls in exit chains", total, deferred)
}

// gateOne checks one body's gates and returns its number of chain members.
func gateOne(t *testing.T, name string, nodes []dataflow.Node, es []dataflow.CfgEdge) int {
	t.Helper()
	entries, exits, chain := 0, 0, 0
	keys := map[string]bool{}
	for _, n := range nodes {
		keys[n.Key] = true
	}
	fwd := map[string]bool{entry: true}
	for k := range closure(es)[entry] {
		fwd[k] = true
	}
	rev := make([]dataflow.CfgEdge, len(es))
	for i, g := range es {
		rev[i] = dataflow.CfgEdge{Src: g.Dst, Dst: g.Src, Kind: g.Kind}
	}
	bwd := map[string]bool{exit: true}
	for k := range closure(rev)[exit] {
		bwd[k] = true
	}
	for _, n := range nodes {
		switch n.Key {
		case entry:
			entries++
			continue
		case exit:
			exits++
			continue
		}
		if !n.Pos.IsValid() || n.End < n.Pos || n.Stmt == nil {
			t.Errorf("%s: node %s has no valid span", name, n.Key)
		}
		if n.Deferred {
			chain++
		}
		if n.Parent != "" {
			if !keys[n.Parent] {
				t.Errorf("%s: satellite %s kept without its parent %s", name, n.Key, n.Parent)
			}
			continue
		}
		if !fwd[n.Key] {
			t.Errorf("%s: node %s unreachable from @entry", name, n.Key)
		}
		if !bwd[n.Key] {
			t.Errorf("%s: node %s cannot reach @exit", name, n.Key)
		}
	}
	if entries != 1 || exits != 1 {
		t.Errorf("%s: %d @entry / %d @exit nodes, want 1 / 1", name, entries, exits)
	}
	if !fwd[exit] {
		t.Errorf("%s: @exit unreachable from @entry", name)
	}
	return chain
}

// gateCallKeys lists every call in body in the L1 recording order (pre-order;
// a go statement's call only; no function literal bodies) with v2 keys.
func gateCallKeys(fset *token.FileSet, body *ast.BlockStmt) []dataflow.CallKey {
	var calls []*ast.CallExpr
	ast.Inspect(body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.GoStmt:
			calls = append(calls, n.Call)
			return false
		case *ast.CallExpr:
			calls = append(calls, n)
		}
		return true
	})
	pos := make([][2]int, len(calls))
	for i, c := range calls {
		p := fset.Position(c.Pos())
		pos[i] = [2]int{p.Line, p.Column}
	}
	keys := v2.CallBodyKeys(pos)
	out := make([]dataflow.CallKey, len(calls))
	for i, c := range calls {
		out[i] = dataflow.CallKey{Pos: c.Pos(), Key: keys[i]}
	}
	return out
}

// ---- canPanic rules (spec §4.3.2) ----

const panicSrc = `package p

type T struct {
	x int
	p *T
}

func (T) M() {}

func f() int { return 0 }

type Celsius float64

type Seq func(func(int) bool)

func _(s []int, p *T, t T, i any, ch chan int, a, b int, fl float64,
	r interface{ Close() error }, sh uint, seq func(func(int) bool), ns Seq, m map[int]int) {
	_ = Celsius(fl)
	_ = f()
	_ = s[a]
	_ = s[a:b]
	_ = i.(int)
	v, ok := i.(int)
	_ = *p
	_ = p.x
	_ = t.x
	ch <- a
	_ = a / b
	_ = a / 2
	_ = a % b
	a /= b
	_ = func() { _ = s[0] }
	var q *T
	_ = <-ch
	_ = []*T{}
	if a > 0 {
		f()
	}
	for i := 0; i < len(s); i++ {
	}
	switch x := i.(type) {
	case *T:
		_ = x
	}
	switch a {
	case f():
	}
	defer f()
	defer t.M()
	defer p.M()
	defer close(ch)
	defer g(s[0])
	go f()
	go g(f())
	_, _, _ = v, ok, q
	_ = r.Close
	defer r.Close()
	go r.Close()
	_ = a << b
	_ = a >> sh
	_ = a << 2
	a <<= b
	a >>= sh
	_ = [2]int(s)
	_ = (*[2]int)(s)
	_ = []int(s)
	for x := range seq {
		_ = x
	}
	for x := range ns {
		_ = x
	}
	for range s {
	}
	_ = m[a]
	_ = recover()
	close(ch)
}

func g(int) {}
`

func TestCanPanic(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "p.go", panicSrc, 0)
	if err != nil {
		t.Fatal(err)
	}
	info := &types.Info{Types: map[ast.Expr]types.TypeAndValue{}, Uses: map[*ast.Ident]types.Object{}, Defs: map[*ast.Ident]types.Object{}, Selections: map[*ast.SelectorExpr]*types.Selection{}}
	if _, err := (&types.Config{}).Check("p", fset, []*ast.File{file}, info); err != nil {
		t.Fatal(err)
	}
	var body []ast.Stmt
	for _, d := range file.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "_" {
			body = fd.Body.List
		}
	}
	want := []bool{
		false, // conversion
		true,  // call
		true,  // index
		true,  // slice
		true,  // type assertion
		false, // comma-ok type assertion
		true,  // dereference
		true,  // selector on a pointer
		false, // selector on a value
		true,  // send
		true,  // non-constant divisor
		false, // constant divisor
		true,  // non-constant %
		true,  // /=
		false, // function literal body not entered
		false, // pointer type in a declaration
		false, // receive
		false, // composite literal of pointer element type
		false, // if header only (body has the call)
		true,  // for header calls len
		false, // type switch guard
		false, // expression switch with no init / tag panic
		false, // defer f(): the call runs at exit
		false, // defer t.M(): value receiver
		true,  // defer p.M(): *p is dereferenced at registration
		false, // defer close(ch): close runs at exit
		true,  // defer g(s[0]): argument evaluated at registration
		false, // go f()
		true,  // go g(f()): argument evaluated here
		false, // _, _, _ = …
		true,  // interface method value (nil receiver)
		true,  // defer r.Close(): interface method value evaluated at registration
		true,  // go r.Close(): likewise
		true,  // shift by a signed non-constant count
		false, // shift by an unsigned count
		false, // shift by a constant
		true,  // <<= by a signed count
		false, // >>= by an unsigned count
		true,  // slice → array conversion
		true,  // slice → array-pointer conversion
		false, // slice → slice conversion
		true,  // range over a func (calls the iterator)
		true,  // range over a named func type
		false, // range over a slice
		true,  // map index (nil map write / index of a map key type that panics)
		true,  // recover()
		true,  // close(ch)
	}
	if len(body) != len(want) {
		t.Fatalf("snippet has %d statements, table %d", len(body), len(want))
	}
	for i, s := range body {
		if got := dataflow.CanPanic(s, info); got != want[i] {
			t.Errorf("stmt %d (line %d): canPanic = %v, want %v", i, fset.Position(s.Pos()).Line, got, want[i])
		}
	}
	// Clause heads inspect their own list: `case *T:` is a type, `case f():`
	// a call.
	tsClause := body[20].(*ast.TypeSwitchStmt).Body.List[0]
	swClause := body[21].(*ast.SwitchStmt).Body.List[0]
	if dataflow.CanPanic(tsClause, info) || !dataflow.CanPanic(swClause, info) {
		t.Errorf("clause heads: type case = %v, call case = %v; want false, true", dataflow.CanPanic(tsClause, info), dataflow.CanPanic(swClause, info))
	}
	// A deferred call node itself (the CallExpr) can panic.
	if !dataflow.CanPanic(body[25].(*ast.DeferStmt).Call, info) {
		t.Errorf("deferred close(ch) call: canPanic = false, want true")
	}
	// The receive row stays false: `_ = <-ch` (stmt 16).
	if dataflow.CanPanic(body[16], info) {
		t.Errorf("receive: canPanic = true, want false")
	}
	// Without type info, every call and selector is conservatively true.
	if !dataflow.CanPanic(body[0], nil) || !dataflow.CanPanic(body[8], nil) {
		t.Errorf("nil info: conversion / value selector must be conservatively true")
	}
}

func TestCFG_EmptyBody(t *testing.T) {
	// func cleanup() {}: falling off the end is a return.
	wantEdges(t, "cleanup", e(entry, exit, "return"))
}

func TestCFG_BodylessCallable(t *testing.T) {
	nodes, keyOf := dataflow.BuildNodes(token.NewFileSet(), nil, nil)
	kept, es := dataflow.BuildCFG(nodes, keyOf, nil, nil)
	if len(es) != 0 || es == nil {
		t.Errorf("nil body: edges = %#v, want empty non-nil", es)
	}
	if len(kept) != 0 || kept == nil {
		t.Errorf("nil body: nodes = %#v, want empty non-nil", kept)
	}
}

// ---- differential test against golang.org/x/tools/go/cfg ----

// isBuiltinPanic reports whether call calls the builtin panic.
func isBuiltinPanic(call *ast.CallExpr, info *types.Info) bool {
	id, ok := ast.Unparen(call.Fun).(*ast.Ident)
	if !ok {
		return false
	}
	b, ok := info.Uses[id].(*types.Builtin)
	return ok && b.Name() == "panic"
}

// diffCase is one function body for the differential test.
type diffCase struct {
	name string
	body *ast.BlockStmt
	info *types.Info
}

func loadDiffCases(t *testing.T, dir string) (*token.FileSet, []diffCase) {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := packages.Load(&packages.Config{
		Mode: packages.NeedName | packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo,
		Dir:  dir,
		Fset: fset,
	}, "./...")
	if err != nil {
		t.Fatalf("load %s: %v", dir, err)
	}
	var out []diffCase
	for _, p := range pkgs {
		if len(p.Errors) > 0 {
			t.Fatalf("load %s: %v", p.PkgPath, p.Errors)
		}
		for _, f := range p.Syntax {
			ast.Inspect(f, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.FuncDecl:
					if n.Body != nil {
						name := n.Name.Name
						if n.Recv != nil && len(n.Recv.List) > 0 {
							name = types.ExprString(n.Recv.List[0].Type) + "." + name
						}
						out = append(out, diffCase{p.PkgPath + "." + name, n.Body, p.TypesInfo})
					}
				case *ast.FuncLit:
					pos := fset.Position(n.Pos())
					out = append(out, diffCase{fmt.Sprintf("%s.func@%d:%d", p.PkgPath, pos.Line, pos.Column), n.Body, p.TypesInfo})
				}
				return true
			})
		}
	}
	return fset, out
}

// TestCFG_Differential checks that our CFG is at least as connected as
// go/cfg's: whenever go/cfg has a path from statement A to statement B, ours
// has a path from A's node to B's node. The reverse is not asserted (ours adds
// synthetic exception edges, panic edges and the defer chain).
//
// go/cfg's mayReturn is false exactly for a call to the builtin panic, which
// in ours has only its exception edge; every other call (os.Exit included)
// returns, as in ours (spec §4.3.2). Only live go/cfg blocks are compared:
// ours prunes what is unreachable from @entry.
//
// Mapping rule (go/cfg block Nodes → our keys), see diffKeyOf:
//   - a statement, or a statement folded into a header (if/for/switch init,
//     for post, type-switch guard) → keyOf[stmt];
//   - a `var` ValueSpec (go/cfg splits a DeclStmt per spec) → its DeclStmt's
//     node;
//   - a header expression (if/for cond, switch tag, range key/value/X) → the
//     header node;
//   - a switch case-list expression → the switch header. go/cfg tests case
//     expressions in sequence (clause i's failed test falls into clause i+1's
//     test); ours fans out header → every clause head, so the case tests are
//     part of the header's dispatch;
//   - a select comm statement → the select header. go/cfg evaluates every
//     comm operation in one block before choosing (its builder's own TODO);
//     ours folds each into its clause head, which are siblings;
//   - a comm clause's receive LHS (go/cfg adds it in the chosen case body) →
//     that clause head;
//   - go/cfg's synthetic fall-off return (a *ast.ReturnStmt not in the
//     source, at body.End()-1) → @exit;
//   - an unlabeled or implicit empty statement → excluded (go/cfg adds every
//     one; ours gives an unlabeled one no node, and leaves an implicit `L: }`
//     unlinked: its label resolves to the continuation per ruling R6). An
//     explicit labeled `L: ;` keeps its node and is compared.
//
// Defers: BuildNodes runs with real call keys (gateCallKeys, the L1
// recording order), so deferred call nodes exist and every exit edge runs
// through the defer chain. go/cfg does not model defers: its DeferStmt is a
// plain statement (mapped to our registration node) and it has no node for
// the deferred call, so a deferred call node is never a compared endpoint
// (diffOne rejects one as a mapped key). Every go/cfg pair ending at its
// fall-off return (→ @exit) is still checked, so @exit must stay reachable
// through the chain from every live statement.
//
// Pairs whose two go/cfg nodes map to the same key are skipped.
func TestCFG_Differential(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(thisFile), "..", "..", "testdata")
	total, pairs, chained := 0, 0, 0
	for _, dir := range []string{"dataflow", "chi"} {
		fset, cases := loadDiffCases(t, filepath.Join(root, dir))
		for _, c := range cases {
			total++
			nodes, keyOf := dataflow.BuildNodes(fset, c.body, gateCallKeys(fset, c.body))
			kept, ours := dataflow.BuildCFG(nodes, keyOf, c.body, c.info)
			checkContract(t, c.name, kept, ours)
			pairs += diffOne(t.Errorf, c.name, c.body, c.info, keyOf, ours)
			for _, n := range kept {
				if n.Deferred {
					chained++
				}
			}
		}
	}
	if total < 100 || pairs < 1000 || chained == 0 {
		t.Errorf("differential covered only %d bodies / %d pairs / %d deferred calls", total, pairs, chained)
	}
	t.Logf("differential: %d bodies, %d reachable pairs checked, %d deferred calls chained", total, pairs, chained)
}

// diffOne reports every go/cfg-reachable pair that ours does not reach and
// returns the number of distinct pairs checked.
func diffOne(errorf func(string, ...any), name string, body *ast.BlockStmt, info *types.Info, keyOf map[ast.Node]string, ours []dataflow.CfgEdge) int {
	g := cfg.New(body, func(call *ast.CallExpr) bool { return !isBuiltinPanic(call, info) })
	km := diffKeyOf(body, keyOf)
	deferred := map[string]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.DeferStmt:
			deferred[keyOf[n.Call]] = true
		}
		return true
	})

	// Map each live block's nodes to our keys, in block order. Dead blocks
	// (code after a return / panic / goto) are pruned from ours.
	blockKeys := make([][]string, len(g.Blocks))
	for _, b := range g.Blocks {
		if !b.Live {
			continue
		}
		for _, n := range b.Nodes {
			k, ok := km[n]
			if r, isRet := n.(*ast.ReturnStmt); !ok && isRet && r.Return == body.End()-1 {
				k, ok = dataflow.ExitKey, true // go/cfg's synthetic fall-off return
			}
			if !ok {
				errorf("%s: go/cfg node %T at %d has no mapped key", name, n, n.Pos())
				continue
			}
			if k == "" { // excluded by the mapping rule
				continue
			}
			if deferred[k] {
				errorf("%s: go/cfg node %T maps to deferred call %s", name, n, k)
				continue
			}
			blockKeys[b.Index] = append(blockKeys[b.Index], k)
		}
	}

	// Block-level transitive closure (paths of length >= 1).
	reachB := make([]map[int32]bool, len(g.Blocks))
	for _, b := range g.Blocks {
		seen := map[int32]bool{}
		stack := append([]*cfg.Block(nil), b.Succs...)
		for len(stack) > 0 {
			x := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if seen[x.Index] {
				continue
			}
			seen[x.Index] = true
			stack = append(stack, x.Succs...)
		}
		reachB[b.Index] = seen
	}

	oursReach := closure(ours)
	type pair struct{ a, b string }
	checked := map[pair]bool{}
	check := func(a, b string) {
		if a == b || checked[pair{a, b}] {
			return
		}
		checked[pair{a, b}] = true
		if !oursReach[a][b] {
			errorf("%s: go/cfg reaches %s → %s; ours does not", name, a, b)
		}
	}
	for _, x := range g.Blocks {
		ks := blockKeys[x.Index]
		for i, a := range ks {
			for _, b := range ks[i+1:] { // later in the same block
				check(a, b)
			}
			for y := range reachB[x.Index] {
				for _, b := range blockKeys[y] {
					check(a, b)
				}
			}
		}
	}
	return len(checked)
}

// closure returns, for every key, the set of keys reachable by a path of
// length >= 1.
func closure(es []dataflow.CfgEdge) map[string]map[string]bool {
	succ := map[string][]string{}
	for _, e := range es {
		succ[e.Src] = append(succ[e.Src], e.Dst)
	}
	out := map[string]map[string]bool{}
	for src := range succ {
		seen := map[string]bool{}
		stack := append([]string(nil), succ[src]...)
		for len(stack) > 0 {
			x := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if seen[x] {
				continue
			}
			seen[x] = true
			stack = append(stack, succ[x]...)
		}
		out[src] = seen
	}
	return out
}

// diffKeyOf extends keyOf with the go/cfg-only nodes per the mapping rule of
// TestCFG_Differential. Function literal bodies are not entered: go/cfg treats
// a FuncLit as an expression.
func diffKeyOf(body *ast.BlockStmt, keyOf map[ast.Node]string) map[ast.Node]string {
	km := make(map[ast.Node]string, len(keyOf))
	for n, k := range keyOf {
		km[n] = k
	}
	put := func(n ast.Node, k string) {
		if n != nil && k != "" {
			km[n] = k
		}
	}
	ast.Inspect(body, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.EmptyStmt:
			// go/cfg adds every empty statement. Ours has a node only for a
			// label target, and an implicit one (`L: }`) is not linked
			// (ruling R6: its label resolves to the continuation). Excluded.
			if s.Implicit || keyOf[s] == "" {
				km[s] = ""
			}
		case *ast.DeclStmt:
			for _, sp := range s.Decl.(*ast.GenDecl).Specs {
				put(sp, keyOf[s])
			}
		case *ast.IfStmt:
			put(s.Cond, keyOf[s])
		case *ast.ForStmt:
			put(s.Cond, keyOf[s])
		case *ast.RangeStmt:
			put(s.X, keyOf[s])
			put(s.Key, keyOf[s])
			put(s.Value, keyOf[s])
		case *ast.SwitchStmt:
			put(s.Tag, keyOf[s])
			for _, c := range s.Body.List {
				for _, x := range c.(*ast.CaseClause).List {
					put(x, keyOf[s])
				}
			}
		case *ast.SelectStmt:
			for _, c := range s.Body.List {
				cc := c.(*ast.CommClause)
				put(cc.Comm, keyOf[s])
				if as, ok := cc.Comm.(*ast.AssignStmt); ok {
					put(as.Lhs[0], keyOf[cc])
				}
			}
		}
		return true
	})
	return km
}

// TestCFG_DifferentialCatchesMissingEdge guards the differential itself: a
// CFG with one edge removed must be reported.
func TestCFG_DifferentialCatchesMissingEdge(t *testing.T) {
	fx := loadFlow(t)
	body := fx.decl(t, "Labeled").Body
	nodes, keyOf := dataflow.BuildNodes(fx.fset, body, nil)
	_, full := dataflow.BuildCFG(nodes, keyOf, body, fx.info)
	var cut []dataflow.CfgEdge // drop every edge into the return
	for _, g := range full {
		if g.Dst != lbRet {
			cut = append(cut, g)
		}
	}
	var errs []string
	diffOne(func(f string, a ...any) { errs = append(errs, fmt.Sprintf(f, a...)) }, "Labeled(cut)", body, fx.info, keyOf, cut)
	if !strings.Contains(strings.Join(errs, "\n"), lbInit+" → "+lbRet) {
		t.Errorf("differential missed the cut edges; got %v", errs)
	}
}
