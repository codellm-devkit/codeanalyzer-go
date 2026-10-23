package dataflow_test

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/tools/go/cfg"
	"golang.org/x/tools/go/packages"

	"github.com/codellm-devkit/codeanalyzer-go/internal/dataflow"
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
)

// edges runs Tasks 5–6 over fixture function fn and checks the output
// contract: edges sorted by (src, dst, kind) with no duplicates.
func edges(t *testing.T, fn string) []dataflow.CfgEdge {
	t.Helper()
	fx := loadFlow(t)
	body := fx.decl(t, fn).Body
	nodes, keyOf := dataflow.BuildNodes(fx.fset, body, fx.callKeys(t, fn))
	es := dataflow.BuildCFG(nodes, keyOf, body, fx.info)
	checkContract(t, fn, nodes, es)
	return es
}

// checkContract asserts the output contract: every endpoint is a node, never
// a satellite or deferred call node (neither is a CFG step here), and the
// edges are sorted by (src, dst, kind) with no duplicates.
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
			} else if n.Parent != "" || n.Deferred {
				t.Errorf("%s: edge %v links satellite/deferred call %s", fn, g, k)
			}
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
	// the CFG leaves it unlinked, for Task 7's pruning.
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

// TestCFG_DeferredCallOffPath: the defer registration statement is on the
// straight-line path; the deferred call nodes are not linked (Task 7 puts
// them in the exit chain).
func TestCFG_DeferredCallOffPath(t *testing.T) {
	wantEdges(t, "EarlyReturnDefer",
		e(entry, "126:2", "fallthrough"),
		e("126:2", "127:2", "fallthrough"),
		e("127:2", "128:2", "fallthrough"),
		e("128:2", "129:3", "true"),
		e("128:2", "131:2", "false"),
		e("129:3", exit, "return"),
		e("131:2", "132:2", "fallthrough"),
		e("132:2", exit, "return"),
	)
}

func TestCFG_EmptyBody(t *testing.T) {
	// func cleanup() {}: falling off the end is a return.
	wantEdges(t, "cleanup", e(entry, exit, "return"))
}

func TestCFG_BodylessCallable(t *testing.T) {
	nodes, keyOf := dataflow.BuildNodes(token.NewFileSet(), nil, nil)
	if es := dataflow.BuildCFG(nodes, keyOf, nil, nil); len(es) != 0 || es == nil {
		t.Errorf("nil body: edges = %#v, want empty non-nil", es)
	}
}

// ---- differential test against golang.org/x/tools/go/cfg ----

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
// synthetic exception edges, and Task 7 adds panic/defer edges).
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
// Calls are passed as nil to BuildNodes: L1 call keys only rename call nodes,
// and the CFG reads statement keys from keyOf, so the shape is unaffected.
// Pairs whose two go/cfg nodes map to the same key are skipped.
func TestCFG_Differential(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(thisFile), "..", "..", "testdata")
	total, pairs := 0, 0
	for _, dir := range []string{"dataflow", "chi"} {
		fset, cases := loadDiffCases(t, filepath.Join(root, dir))
		for _, c := range cases {
			total++
			nodes, keyOf := dataflow.BuildNodes(fset, c.body, nil)
			ours := dataflow.BuildCFG(nodes, keyOf, c.body, c.info)
			checkContract(t, c.name, nodes, ours)
			pairs += diffOne(t.Errorf, c.name, c.body, keyOf, ours)
		}
	}
	if total < 100 || pairs < 1000 {
		t.Errorf("differential covered only %d bodies / %d pairs", total, pairs)
	}
	t.Logf("differential: %d bodies, %d reachable pairs checked", total, pairs)
}

// diffOne reports every go/cfg-reachable pair that ours does not reach and
// returns the number of distinct pairs checked.
func diffOne(errorf func(string, ...any), name string, body *ast.BlockStmt, keyOf map[ast.Node]string, ours []dataflow.CfgEdge) int {
	g := cfg.New(body, func(*ast.CallExpr) bool { return true })
	km := diffKeyOf(body, keyOf)

	// Map each block's nodes to our keys, in block order.
	blockKeys := make([][]string, len(g.Blocks))
	for _, b := range g.Blocks {
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
	full := dataflow.BuildCFG(nodes, keyOf, body, fx.info)
	var cut []dataflow.CfgEdge // drop every edge into the return
	for _, g := range full {
		if g.Dst != lbRet {
			cut = append(cut, g)
		}
	}
	var errs []string
	diffOne(func(f string, a ...any) { errs = append(errs, fmt.Sprintf(f, a...)) }, "Labeled(cut)", body, keyOf, cut)
	if !strings.Contains(strings.Join(errs, "\n"), lbInit+" → "+lbRet) {
		t.Errorf("differential missed the cut edges; got %v", errs)
	}
}
