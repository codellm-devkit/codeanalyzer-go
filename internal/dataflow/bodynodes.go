package dataflow

import (
	"go/ast"
	"go/token"
	"sort"
	"strconv"

	v2 "github.com/codellm-devkit/codeanalyzer-go/internal/schema/v2"
)

// CallKey is one L1 call node: the call expression's position and its body
// key. The caller builds the list from the callable's L1 call sites with
// v2.CallBodyKeys, in recording order, so L3 call keys are byte-identical to
// the L1 ones; BuildNodes never derives a call key itself.
type CallKey struct {
	Pos token.Pos
	Key string
}

// BuildNodes maps a callable body onto its L3 body nodes (spec §4.2) and
// returns them with a map from AST node to node key.
//
// Node rules:
//   - if → branch; for / for-range → loop; switch / type switch / select →
//     switch; return → return; every other statement → statement. The init
//     statement of if/for/switch, the post statement of a 3-clause for, a type
//     switch's guard and a comm clause's send/receive fold into their header or
//     clause-head node; they get no node of their own.
//   - each case / default / comm clause head is a statement node keyed at the
//     clause's Pos(); its span ends at the clause's colon.
//   - a bare call statement (ExprStmt whose expression is an L1 call) and a
//     `go` statement are the call node: kind call, keyed with the L1 call key.
//   - `defer f(x)` is two nodes (§4.3.1): a statement node at the `defer`
//     keyword and the L1 call node with Deferred set. Calls in the deferred
//     call's receiver and arguments run at registration and are satellites of
//     the defer statement.
//   - every other L1 call is a satellite call node whose Parent is the key of
//     the innermost enclosing statement / header / clause-head node.
//   - blocks and labels get no node (a label's inner statement does); an empty
//     statement gets one only as a label target. Function literal bodies are
//     skipped: closures are their own callables.
//
// Keys: call nodes take their CallKey.Key. Statement nodes take
// v2.LocalID(line, col) of their anchor; a statement starting with a call
// (`f().x = v`) would collide with the L1 call key, so it takes the next free
// "/n" suffix instead and the L1 key is never moved.
//
// Order (deterministic): @entry first, then the real nodes in source order
// (by Pos; co-positioned nodes in traversal order — a statement before its
// calls, an outer call before the calls nested in it), then @exit last. The
// synthetic nodes have NoPos and a nil Stmt. A nil body yields an empty slice
// and an empty map (no entry/exit: a bodyless callable has no flow).
//
// The returned map keys every node's Stmt to its key. It also maps the AST
// nodes folded into a node to that node's key: a bare or `go` call's
// *ast.CallExpr, an if/for/switch init, a for post, a type switch guard, a comm
// clause's statement, and a *ast.LabeledStmt (to its inner statement's node).
func BuildNodes(fset *token.FileSet, body *ast.BlockStmt, calls []CallKey) ([]Node, map[ast.Node]string) {
	if body == nil {
		return []Node{}, map[ast.Node]string{}
	}
	b := &builder{
		fset:    fset,
		callKey: assignCallKeys(body, calls),
		taken:   make(map[string]bool, len(calls)),
		keyOf:   map[ast.Node]string{},
	}
	for _, c := range calls {
		b.taken[c.Key] = true
	}
	b.stmtList(body.List)

	sort.SliceStable(b.nodes, func(i, j int) bool { return b.nodes[i].Pos < b.nodes[j].Pos })
	out := make([]Node, 0, len(b.nodes)+2)
	out = append(out, Node{Key: EntryKey, Kind: KindEntry})
	out = append(out, b.nodes...)
	out = append(out, Node{Key: ExitKey, Kind: KindExit})
	return out, b.keyOf
}

// assignCallKeys pairs each call expression with its L1 key. It walks the body
// in the L1 recording order (pre-order; a `go` statement's call only, not its
// arguments; no function literal bodies) and takes, for each call, the next
// unused key recorded at its position — so co-positioned chained calls get
// "l:c", "l:c/2", … outermost first, exactly as L1 assigned them.
func assignCallKeys(body *ast.BlockStmt, calls []CallKey) map[*ast.CallExpr]string {
	byPos := map[token.Pos][]string{}
	for _, c := range calls {
		byPos[c.Pos] = append(byPos[c.Pos], c.Key)
	}
	out := map[*ast.CallExpr]string{}
	take := func(call *ast.CallExpr) {
		if q := byPos[call.Pos()]; len(q) > 0 {
			out[call] = q[0]
			byPos[call.Pos()] = q[1:]
		}
	}
	ast.Inspect(body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.GoStmt:
			take(n.Call)
			return false
		case *ast.FuncLit:
			return false
		case *ast.CallExpr:
			take(n)
		}
		return true
	})
	return out
}

type builder struct {
	fset    *token.FileSet
	callKey map[*ast.CallExpr]string
	taken   map[string]bool
	keyOf   map[ast.Node]string
	nodes   []Node
}

func (b *builder) stmtList(list []ast.Stmt) {
	for _, s := range list {
		b.stmt(s)
	}
}

func (b *builder) stmt(s ast.Stmt) {
	switch s := s.(type) {
	case *ast.BlockStmt:
		b.stmtList(s.List)

	case *ast.LabeledStmt:
		if e, ok := s.Stmt.(*ast.EmptyStmt); ok {
			b.add(KindStatement, e, s.Stmt.Pos(), s.Stmt.End())
		} else {
			b.stmt(s.Stmt)
		}
		if k, ok := b.keyOf[s.Stmt]; ok {
			b.keyOf[s] = k
		}

	case *ast.EmptyStmt:
		// No node: gofmt'd code has an empty statement only as a label target.

	case *ast.ExprStmt:
		if call, ok := ast.Unparen(s.X).(*ast.CallExpr); ok {
			if key, ok := b.callKey[call]; ok {
				b.addCall(key, s, call)
				b.satellites(key, s.X, call)
				return
			}
		}
		b.simple(s)

	case *ast.GoStmt:
		if key, ok := b.callKey[s.Call]; ok {
			b.addCall(key, s, s.Call)
			b.satellites(key, s.Call, s.Call)
			return
		}
		b.simple(s)

	case *ast.DeferStmt:
		reg := b.add(KindStatement, s, s.Pos(), s.End())
		if key, ok := b.callKey[s.Call]; ok {
			b.nodes = append(b.nodes, Node{Key: key, Kind: KindCall, Pos: s.Call.Pos(), End: s.Call.End(), Stmt: s.Call, Deferred: true})
			b.keyOf[s.Call] = key
		}
		b.satellites(reg, s.Call.Fun, nil)
		for _, a := range s.Call.Args {
			b.satellites(reg, a, nil)
		}

	case *ast.IfStmt:
		k := b.add(KindBranch, s, s.Pos(), s.End())
		b.folded(k, s.Init)
		b.satellites(k, s.Cond, nil)
		b.stmt(s.Body)
		if s.Else != nil {
			b.stmt(s.Else)
		}

	case *ast.ForStmt:
		k := b.add(KindLoop, s, s.Pos(), s.End())
		b.folded(k, s.Init)
		b.satellites(k, s.Cond, nil)
		b.folded(k, s.Post)
		b.stmt(s.Body)

	case *ast.RangeStmt:
		k := b.add(KindLoop, s, s.Pos(), s.End())
		b.satellites(k, s.Key, nil)
		b.satellites(k, s.Value, nil)
		b.satellites(k, s.X, nil)
		b.stmt(s.Body)

	case *ast.SwitchStmt:
		k := b.add(KindSwitch, s, s.Pos(), s.End())
		b.folded(k, s.Init)
		b.satellites(k, s.Tag, nil)
		b.caseClauses(s.Body)

	case *ast.TypeSwitchStmt:
		k := b.add(KindSwitch, s, s.Pos(), s.End())
		b.folded(k, s.Init)
		b.folded(k, s.Assign)
		b.caseClauses(s.Body)

	case *ast.SelectStmt:
		b.add(KindSwitch, s, s.Pos(), s.End())
		for _, c := range s.Body.List {
			cc := c.(*ast.CommClause)
			k := b.add(KindStatement, cc, cc.Pos(), cc.Colon+1)
			b.folded(k, cc.Comm)
			b.stmtList(cc.Body)
		}

	case *ast.ReturnStmt:
		k := b.add(KindReturn, s, s.Pos(), s.End())
		b.satellites(k, s, nil)

	default:
		// assign, :=, var decl, inc/dec, send, non-call expression statement,
		// break / continue / goto / fallthrough.
		b.simple(s)
	}
}

func (b *builder) caseClauses(body *ast.BlockStmt) {
	for _, c := range body.List {
		cc := c.(*ast.CaseClause)
		k := b.add(KindStatement, cc, cc.Pos(), cc.Colon+1)
		for _, e := range cc.List {
			b.satellites(k, e, nil)
		}
		b.stmtList(cc.Body)
	}
}

// simple adds a statement node for a statement with no nested statements and
// makes every call in it a satellite.
func (b *builder) simple(s ast.Stmt) {
	k := b.add(KindStatement, s, s.Pos(), s.End())
	b.satellites(k, s, nil)
}

// folded attributes a header's init / post / guard statement to the header
// node k: no node of its own, its calls are k's satellites.
func (b *builder) folded(k string, s ast.Stmt) {
	if s == nil {
		return
	}
	b.keyOf[s] = k
	b.satellites(k, s, nil)
}

// add appends a non-call node anchored at pos and returns its key: the
// position's LocalID, or the next free "/n" suffix when an L1 call (or an
// earlier node) already holds it.
func (b *builder) add(kind NodeKind, n ast.Node, pos, end token.Pos) string {
	p := b.fset.Position(pos)
	base := v2.LocalID(p.Line, p.Column)
	key := base
	for i := 2; b.taken[key]; i++ {
		key = base + "/" + strconv.Itoa(i)
	}
	b.taken[key] = true
	b.nodes = append(b.nodes, Node{Key: key, Kind: kind, Pos: pos, End: end, Stmt: n})
	b.keyOf[n] = key
	return key
}

// addCall appends a statement that is its call node (a bare call or `go`).
func (b *builder) addCall(key string, s ast.Stmt, call *ast.CallExpr) {
	b.nodes = append(b.nodes, Node{Key: key, Kind: KindCall, Pos: s.Pos(), End: s.End(), Stmt: s})
	b.keyOf[s] = key
	b.keyOf[call] = key
}

// satellites adds a call node with Parent = parent for every L1 call inside n,
// except skip (the statement's own call node). Function literal bodies are
// not entered.
func (b *builder) satellites(parent string, n ast.Node, skip *ast.CallExpr) {
	if n == nil {
		return
	}
	ast.Inspect(n, func(x ast.Node) bool {
		switch x := x.(type) {
		case *ast.FuncLit:
			return false
		case *ast.CallExpr:
			if key, ok := b.callKey[x]; ok && x != skip {
				b.nodes = append(b.nodes, Node{Key: key, Kind: KindCall, Pos: x.Pos(), End: x.End(), Stmt: x, Parent: parent})
				b.keyOf[x] = key
			}
		}
		return true
	})
}
