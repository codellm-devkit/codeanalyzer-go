package dataflow_test

import (
	"go/ast"
	"go/token"
	"go/types"
	"path/filepath"
	"reflect"
	"runtime"
	"sync"
	"testing"

	"github.com/codellm-devkit/codeanalyzer-go/internal/dataflow"
	"github.com/codellm-devkit/codeanalyzer-go/internal/schema"
	v2 "github.com/codellm-devkit/codeanalyzer-go/internal/schema/v2"
	"github.com/codellm-devkit/codeanalyzer-go/internal/syntactic_analysis"
)

// flowFixture is the testdata/dataflow module loaded once through the real
// symbol-table builder, so call keys come from the L1 call-site list.
type flowFixture struct {
	fset  *token.FileSet
	info  *types.Info
	decls map[string]*ast.FuncDecl
	sites map[string][]schema.GoCallsite
}

var (
	fixtureOnce sync.Once
	fixture     *flowFixture
	fixtureErr  error
)

func loadFlow(t *testing.T) *flowFixture {
	t.Helper()
	fixtureOnce.Do(func() {
		_, thisFile, _, _ := runtime.Caller(0)
		dir := filepath.Join(filepath.Dir(thisFile), "..", "..", "testdata", "dataflow")
		b := syntactic_analysis.NewSymbolTableBuilder(dir)
		st, err := b.Build(nil, true)
		if err != nil {
			fixtureErr = err
			return
		}
		fx := &flowFixture{fset: b.Fset(), decls: map[string]*ast.FuncDecl{}, sites: map[string][]schema.GoCallsite{}}
		for _, fn := range st[filepath.Join("flow", "flow.go")].Functions {
			fx.sites[fn.Name] = fn.CallSites
		}
		pkg := b.Pkgs()["example.com/dataflow/flow"]
		fx.info = pkg.TypesInfo
		for _, f := range pkg.Syntax {
			for _, d := range f.Decls {
				if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil {
					fx.decls[fd.Name.Name] = fd
				}
			}
		}
		fixture = fx
	})
	if fixtureErr != nil {
		t.Fatalf("build fixture: %v", fixtureErr)
	}
	return fixture
}

// callKeys builds the L1 call-node keys exactly as the driver does: keys from
// v2.CallBodyKeys over the call sites' [line, col], in recording order, each
// with its site's Lparen. BuildNodes pairs them with the AST calls by Lparen.
func (fx *flowFixture) callKeys(t *testing.T, fn string) []dataflow.UnitCall {
	t.Helper()
	sites := fx.sites[fn]
	pos := make([][2]int, len(sites))
	for i, cs := range sites {
		pos[i] = [2]int{cs.StartLine, cs.StartColumn}
	}
	keys := v2.CallBodyKeys(pos)
	out := make([]dataflow.UnitCall, len(sites))
	for i, cs := range sites {
		out[i] = dataflow.UnitCall{Key: keys[i], Lparen: cs.Lparen}
	}
	return out
}

func (fx *flowFixture) decl(t *testing.T, fn string) *ast.FuncDecl {
	t.Helper()
	d, ok := fx.decls[fn]
	if !ok {
		t.Fatalf("fixture has no function %s", fn)
	}
	return d
}

// build runs BuildNodes over fn and indexes the nodes by key, failing on a
// duplicate key.
func build(t *testing.T, fn string) ([]dataflow.Node, map[string]dataflow.Node, map[ast.Node]string) {
	t.Helper()
	fx := loadFlow(t)
	nodes, keyOf := dataflow.BuildNodes(fx.fset, fx.decl(t, fn).Body, fx.callKeys(t, fn))
	byKey := map[string]dataflow.Node{}
	for _, n := range nodes {
		if _, dup := byKey[n.Key]; dup {
			t.Fatalf("%s: duplicate key %s", fn, n.Key)
		}
		byKey[n.Key] = n
	}
	return nodes, byKey, keyOf
}

func wantKind(t *testing.T, fn string, byKey map[string]dataflow.Node, key string, kind dataflow.NodeKind) dataflow.Node {
	t.Helper()
	n, ok := byKey[key]
	if !ok {
		t.Fatalf("%s: no node %s; have %v", fn, key, keysOf(byKey))
	}
	if n.Kind != kind {
		t.Errorf("%s: node %s kind = %q, want %q", fn, key, n.Kind, kind)
	}
	return n
}

func wantAbsent(t *testing.T, fn string, byKey map[string]dataflow.Node, key, why string) {
	t.Helper()
	if _, ok := byKey[key]; ok {
		t.Errorf("%s: unexpected node %s (%s)", fn, key, why)
	}
}

func keysOf(m map[string]dataflow.Node) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

func TestBodyNodes_KindsPerConstruct(t *testing.T) {
	// IfElse: the init statement folds into the branch header.
	_, ie, _ := build(t, "IfElse")
	wantKind(t, "IfElse", ie, "16:2", dataflow.KindBranch)
	wantAbsent(t, "IfElse", ie, "16:5", "if init folds into the header")
	wantKind(t, "IfElse", ie, "17:3", dataflow.KindReturn)
	wantKind(t, "IfElse", ie, "19:3", dataflow.KindStatement)
	wantKind(t, "IfElse", ie, "21:2", dataflow.KindReturn)
	if len(ie) != 6 { // @entry, if, return, assign, return, @exit
		t.Errorf("IfElse: %d nodes, want 6: %v", len(ie), keysOf(ie))
	}

	// ForLoop: init and post fold into the loop header.
	_, fl, _ := build(t, "ForLoop")
	wantKind(t, "ForLoop", fl, "26:2", dataflow.KindStatement)
	wantKind(t, "ForLoop", fl, "27:2", dataflow.KindLoop)
	wantAbsent(t, "ForLoop", fl, "27:6", "for init folds into the header")
	wantAbsent(t, "ForLoop", fl, "27:21", "for post folds into the header")
	wantKind(t, "ForLoop", fl, "28:3", dataflow.KindStatement)
	wantKind(t, "ForLoop", fl, "30:2", dataflow.KindReturn)

	_, inf, _ := build(t, "Infinite")
	wantKind(t, "Infinite", inf, "35:2", dataflow.KindLoop)

	_, rg, _ := build(t, "Range")
	wantKind(t, "Range", rg, "46:2", dataflow.KindLoop)

	// Switch: header, one statement per clause head, fallthrough statement.
	_, sw, _ := build(t, "Switch")
	wantKind(t, "Switch", sw, "55:2", dataflow.KindSwitch)
	for _, k := range []string{"56:2", "59:2", "61:2", "57:3", "58:3", "60:3", "62:3"} {
		wantKind(t, "Switch", sw, k, dataflow.KindStatement)
	}
	wantKind(t, "Switch", sw, "64:2", dataflow.KindReturn)

	// TypeSwitch: the `t := v.(type)` guard folds into the header.
	_, ts, _ := build(t, "TypeSwitch")
	wantKind(t, "TypeSwitch", ts, "69:2", dataflow.KindSwitch)
	wantAbsent(t, "TypeSwitch", ts, "69:9", "type-switch guard folds into the header")
	for _, k := range []string{"70:2", "73:2", "75:2"} {
		wantKind(t, "TypeSwitch", ts, k, dataflow.KindStatement)
	}
	wantKind(t, "TypeSwitch", ts, "72:3", dataflow.KindReturn)

	// Select2: comm clause heads are statements; the comm stmt folds into them.
	_, sel, _ := build(t, "Select2")
	wantKind(t, "Select2", sel, "82:2", dataflow.KindSwitch)
	wantKind(t, "Select2", sel, "83:2", dataflow.KindStatement)
	wantKind(t, "Select2", sel, "85:2", dataflow.KindStatement)
	wantAbsent(t, "Select2", sel, "83:7", "comm stmt folds into the clause head")
	wantAbsent(t, "Select2", sel, "85:7", "comm stmt folds into the clause head")
	wantKind(t, "Select2", sel, "84:3", dataflow.KindReturn)

	_, es, _ := build(t, "EmptySelect")
	wantKind(t, "EmptySelect", es, "121:2", dataflow.KindSwitch)

	// Labeled: the label gets no node; its inner for does.
	_, lb, _ := build(t, "Labeled")
	wantAbsent(t, "Labeled", lb, "93:1", "a label is not a node")
	wantKind(t, "Labeled", lb, "94:2", dataflow.KindLoop)
	wantKind(t, "Labeled", lb, "97:5", dataflow.KindStatement)  // continue outer
	wantKind(t, "Labeled", lb, "100:5", dataflow.KindStatement) // break outer

	// GotoBack: the goto target is the label's inner statement.
	_, gb, keyOf := build(t, "GotoBack")
	inc := wantKind(t, "GotoBack", gb, "112:2", dataflow.KindStatement)
	wantKind(t, "GotoBack", gb, "114:3", dataflow.KindStatement)
	for n, k := range keyOf {
		if ls, ok := n.(*ast.LabeledStmt); ok && ls.Label.Name == "loop" && k != inc.Key {
			t.Errorf("GotoBack: keyOf[label loop] = %q, want %q", k, inc.Key)
		}
	}

	// Synthetic nodes bracket every body.
	nodes, _, _ := build(t, "IfElse")
	if first, last := nodes[0], nodes[len(nodes)-1]; first.Key != dataflow.EntryKey || first.Kind != dataflow.KindEntry ||
		last.Key != dataflow.ExitKey || last.Kind != dataflow.KindExit {
		t.Errorf("IfElse: want @entry first and @exit last, got %s ... %s", first.Key, last.Key)
	}
	for _, n := range []dataflow.Node{nodes[0], nodes[len(nodes)-1]} {
		if n.Pos != token.NoPos || n.Stmt != nil {
			t.Errorf("synthetic %s has Pos %v / Stmt %T", n.Key, n.Pos, n.Stmt)
		}
	}
	for i := 2; i < len(nodes)-1; i++ {
		if nodes[i].Pos < nodes[i-1].Pos {
			t.Errorf("IfElse: nodes out of source order at %s", nodes[i].Key)
		}
	}
}

func TestBodyNodes_BareCallSharesKey(t *testing.T) {
	fx := loadFlow(t)
	l1 := map[string]bool{}
	for _, c := range fx.callKeys(t, "EarlyReturnDefer") {
		l1[c.Key] = true
	}
	_, byKey, keyOf := build(t, "EarlyReturnDefer")
	n := wantKind(t, "EarlyReturnDefer", byKey, "131:2", dataflow.KindCall)
	if !l1["131:2"] {
		t.Fatalf("131:2 is not an L1 call key: %v", l1)
	}
	if _, ok := n.Stmt.(*ast.ExprStmt); !ok {
		t.Errorf("bare call node Stmt = %T, want *ast.ExprStmt", n.Stmt)
	}
	if n.Parent != "" || n.Deferred {
		t.Errorf("bare call node Parent=%q Deferred=%v, want \"\"/false", n.Parent, n.Deferred)
	}
	if keyOf[n.Stmt] != "131:2" {
		t.Errorf("keyOf[ExprStmt] = %q, want 131:2", keyOf[n.Stmt])
	}

	// `go work(f(x))` is the call node, keyed at the call; f(x), evaluated
	// at the go statement, is its satellite.
	_, sp, _ := build(t, "Spawn")
	g := wantKind(t, "Spawn", sp, "202:5", dataflow.KindCall)
	if _, ok := g.Stmt.(*ast.GoStmt); !ok {
		t.Errorf("go node Stmt = %T, want *ast.GoStmt", g.Stmt)
	}
	wantAbsent(t, "Spawn", sp, "202:2", "go statement shares the call's key")
	if arg := wantKind(t, "Spawn", sp, "202:10", dataflow.KindCall); arg.Parent != "202:5" {
		t.Errorf("Spawn: f(x) Parent = %q, want the go node 202:5", arg.Parent)
	}

	// s.A().B(): the outer call (L1 "234:2") is the statement's node; the
	// co-positioned inner s.A() (L1 "234:2/2") is its satellite.
	_, ch, _ := build(t, "Chain")
	if outer := wantKind(t, "Chain", ch, "234:2", dataflow.KindCall); outer.Parent != "" {
		t.Errorf("Chain: outer call Parent = %q, want \"\"", outer.Parent)
	}
	if inner := wantKind(t, "Chain", ch, "234:2/2", dataflow.KindCall); inner.Parent != "234:2" {
		t.Errorf("Chain: s.A() Parent = %q, want 234:2", inner.Parent)
	}

	// panic("negative") is a bare call statement too: it keeps kind call.
	_, pn, _ := build(t, "Panics")
	wantKind(t, "Panics", pn, "138:3", dataflow.KindCall)
	if len(sp) != 4 { // @entry, go node, f(x) satellite, @exit
		t.Errorf("Spawn: %d nodes, want 4: %v", len(sp), keysOf(sp))
	}
}

func TestBodyNodes_DeferIsTwoNodes(t *testing.T) {
	_, byKey, keyOf := build(t, "EarlyReturnDefer")
	reg := wantKind(t, "EarlyReturnDefer", byKey, "126:2", dataflow.KindStatement)
	if _, ok := reg.Stmt.(*ast.DeferStmt); !ok {
		t.Errorf("registration Stmt = %T, want *ast.DeferStmt", reg.Stmt)
	}
	call := wantKind(t, "EarlyReturnDefer", byKey, "126:8", dataflow.KindCall)
	if !call.Deferred {
		t.Errorf("126:8 not marked Deferred")
	}
	if _, ok := call.Stmt.(*ast.CallExpr); !ok {
		t.Errorf("deferred call Stmt = %T, want *ast.CallExpr", call.Stmt)
	}
	if keyOf[call.Stmt] != "126:8" || keyOf[reg.Stmt] != "126:2" {
		t.Errorf("keyOf: defer=%q call=%q", keyOf[reg.Stmt], keyOf[call.Stmt])
	}

	// defer work(g(a)): g(a) is evaluated at registration, so it is a
	// satellite of the defer statement; work(...) is the deferred call.
	wantKind(t, "EarlyReturnDefer", byKey, "127:2", dataflow.KindStatement)
	if w := wantKind(t, "EarlyReturnDefer", byKey, "127:8", dataflow.KindCall); !w.Deferred {
		t.Errorf("127:8 not marked Deferred")
	}
	inner := wantKind(t, "EarlyReturnDefer", byKey, "127:13", dataflow.KindCall)
	if inner.Deferred || inner.Parent != "127:2" {
		t.Errorf("g(a): Deferred=%v Parent=%q, want false/127:2", inner.Deferred, inner.Parent)
	}
	for _, k := range []string{"126:8", "127:8"} {
		if byKey[k].Parent != "" {
			t.Errorf("deferred call %s Parent = %q, want \"\"", k, byKey[k].Parent)
		}
	}
	if byKey["131:2"].Deferred {
		t.Errorf("non-deferred cleanup() marked Deferred")
	}
}

func TestBodyNodes_NestedCallHasParent(t *testing.T) {
	_, byKey, _ := build(t, "Nested")
	wantKind(t, "Nested", byKey, "196:2", dataflow.KindStatement)
	for _, k := range []string{"196:7", "196:9"} {
		if n := wantKind(t, "Nested", byKey, k, dataflow.KindCall); n.Parent != "196:2" {
			t.Errorf("call %s Parent = %q, want 196:2", k, n.Parent)
		}
	}

	// A call in an if header is a satellite of the header.
	_, ie, _ := build(t, "EarlyReturnDefer")
	if ie["128:2"].Kind != dataflow.KindBranch {
		t.Errorf("128:2 kind = %q, want branch", ie["128:2"].Kind)
	}

	// The closure body is its own callable: no nodes from inside it, and the
	// call h() is a satellite of the return.
	_, cl, _ := build(t, "Closure")
	wantAbsent(t, "Closure", cl, "217:3", "closure body is skipped")
	wantAbsent(t, "Closure", cl, "217:10", "closure body is skipped")
	if n := wantKind(t, "Closure", cl, "219:9", dataflow.KindCall); n.Parent != "219:2" {
		t.Errorf("h() Parent = %q, want 219:2", n.Parent)
	}
}

// TestBodyNodes_StatementCoPositionedWithCall locks the collision rule: a
// statement that starts with a call (get().f.g.x = v) leaves the L1 call key
// "l:c" untouched and takes the next free suffix.
func TestBodyNodes_StatementCoPositionedWithCall(t *testing.T) {
	_, byKey, _ := build(t, "CallLHS")
	call := wantKind(t, "CallLHS", byKey, "211:2", dataflow.KindCall)
	stmt := wantKind(t, "CallLHS", byKey, "211:2/2", dataflow.KindStatement)
	if _, ok := stmt.Stmt.(*ast.AssignStmt); !ok {
		t.Errorf("211:2/2 Stmt = %T, want *ast.AssignStmt", stmt.Stmt)
	}
	if call.Parent != "211:2/2" {
		t.Errorf("get() Parent = %q, want 211:2/2", call.Parent)
	}
}

// TestBodyNodes_CallKeysMatchL1 locks ruling R1: every L1 call key appears as
// exactly one node key, for every fixture function.
func TestBodyNodes_CallKeysMatchL1(t *testing.T) {
	fx := loadFlow(t)
	for fn := range fx.decls {
		_, byKey, _ := build(t, fn)
		for _, c := range fx.callKeys(t, fn) {
			if n, ok := byKey[c.Key]; !ok || n.Kind != dataflow.KindCall {
				t.Errorf("%s: L1 call key %s has no call node", fn, c.Key)
			}
		}
	}
}

func TestBodyNodes_BodylessCallableEmitsNothing(t *testing.T) {
	nodes, keyOf := dataflow.BuildNodes(token.NewFileSet(), nil, nil)
	if len(nodes) != 0 || len(keyOf) != 0 {
		t.Errorf("nil body: %d nodes, %d keys; want none", len(nodes), len(keyOf))
	}
	if nodes == nil || keyOf == nil {
		t.Errorf("nil body: want empty (non-nil) slice and map")
	}
}

// TestBuildNodes_PairsByLparenNotIndex: BuildNodes pairs each L1 call key
// with the call whose Lparen it carries, so the order of the calls slice is
// irrelevant: a permuted slice keys every call exactly as the recording-order
// slice does — co-positioned chained calls (s.A().B() at l:c, s.A() at
// l:c/2) included.
func TestBuildNodes_PairsByLparenNotIndex(t *testing.T) {
	fx := loadFlow(t)
	for _, fn := range []string{"EarlyReturnDefer", "GoArgs", "Chain", "Nested", "Spawn"} {
		body := fx.decl(t, fn).Body
		calls := fx.callKeys(t, fn)
		if len(calls) < 2 {
			t.Fatalf("%s: %d calls, want ≥ 2 to permute", fn, len(calls))
		}
		rev := make([]dataflow.UnitCall, len(calls))
		for i, c := range calls {
			rev[len(calls)-1-i] = c
		}
		nodes, keyOf := dataflow.BuildNodes(fx.fset, body, calls)
		nodes2, keyOf2 := dataflow.BuildNodes(fx.fset, body, rev)
		if !reflect.DeepEqual(nodes, nodes2) || !reflect.DeepEqual(keyOf, keyOf2) {
			t.Errorf("%s: permuted calls build different nodes", fn)
		}
		want := map[token.Pos]string{}
		for _, c := range calls {
			want[c.Lparen] = c.Key
		}
		n := 0
		for node, key := range keyOf2 {
			if call, ok := node.(*ast.CallExpr); ok {
				n++
				if want[call.Lparen] != key {
					t.Errorf("%s: call at %v keyed %s, want %s", fn, fx.fset.Position(call.Pos()), key, want[call.Lparen])
				}
			}
		}
		if n != len(calls) {
			t.Errorf("%s: %d call expressions keyed, want %d", fn, n, len(calls))
		}
	}
}
