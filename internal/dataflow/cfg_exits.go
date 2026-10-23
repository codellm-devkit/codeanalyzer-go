package dataflow

import (
	"go/ast"
	"go/token"
	"go/types"
	"sort"
)

// EdgeDeferResume is the Go add (spec §4.3.1): a deferred call's normal
// completion resuming the exit chain (the next deferred call, or @exit).
const EdgeDeferResume = "defer_resume"

// finishExits applies the exit passes to the structured CFG in l.edges and
// returns the kept nodes (in nodes' order) and the sorted edges:
//
//  1. panic edges: every canPanic node gets exception → @exit; a panic(...)
//     statement keeps only that edge.
//  2. defer chain: the registered deferred calls in reverse source order are
//     D1..Dn. Every edge into @exit is redirected to D1 with its kind kept;
//     Di → Di+1 and Dn → @exit are defer_resume, plus exception when Di can
//     panic. A deferred call is registered when its defer statement is
//     reachable from @entry; one that never is joins no chain.
//  3. pruning: nodes unreachable from @entry are dropped (the chain, @entry
//     and @exit are always kept), with their edges and satellites.
//  4. exit reachability: each set of nodes that cannot reach @exit — a
//     bottom strongly connected component of the graph — gets one synthetic
//     exception edge to the exit target (D1, else @exit) from its header,
//     the component's first node in depth-first order from @entry.
//  5. dedup (the edge set) and sort.
func finishExits(l *linker, nodes []Node, body *ast.BlockStmt, info *types.Info) ([]Node, []CfgEdge) {
	// 1. Panic edges. Satellite and deferred call nodes are not CFG steps
	// here; a satellite's panic is its parent statement's.
	panics := map[string]bool{}
	for _, n := range nodes {
		if n.Stmt == nil || n.Parent != "" || n.Deferred {
			continue
		}
		if isPanicStmt(n.Stmt, info) {
			panics[n.Key] = true
		} else if canPanic(n.Stmt, info) {
			l.edges[CfgEdge{Src: n.Key, Dst: ExitKey, Kind: EdgeException}] = true
		}
	}
	for e := range l.edges {
		if panics[e.Src] {
			delete(l.edges, e)
		}
	}
	for k := range panics {
		l.edges[CfgEdge{Src: k, Dst: ExitKey, Kind: EdgeException}] = true
	}

	// 2. Defer chain over the registered deferred calls.
	reach := reachable(l.edges, EntryKey)
	regOf := registrations(body, l.keyOf)
	var chain []Node
	for i := len(nodes) - 1; i >= 0; i-- {
		if n := nodes[i]; n.Deferred && reach[regOf[n.Stmt]] {
			chain = append(chain, n)
		}
	}
	exitTarget := ExitKey
	if len(chain) > 0 {
		exitTarget = chain[0].Key
		for e := range l.edges {
			if e.Dst == ExitKey {
				delete(l.edges, e)
				l.edges[CfgEdge{Src: e.Src, Dst: exitTarget, Kind: e.Kind}] = true
			}
		}
		for i, d := range chain {
			next := ExitKey
			if i+1 < len(chain) {
				next = chain[i+1].Key
			}
			l.edges[CfgEdge{Src: d.Key, Dst: next, Kind: EdgeDeferResume}] = true
			if canPanic(d.Stmt, info) {
				l.edges[CfgEdge{Src: d.Key, Dst: next, Kind: EdgeException}] = true
			}
		}
	}

	// 3. Pruning.
	keep := map[string]bool{EntryKey: true, ExitKey: true}
	for k := range reach {
		keep[k] = true
	}
	for _, d := range chain {
		keep[d.Key] = true
	}
	for e := range l.edges {
		if !keep[e.Src] {
			delete(l.edges, e)
		}
	}
	kept := make([]Node, 0, len(nodes))
	for _, n := range nodes {
		if keep[n.Key] && n.Parent == "" || n.Parent != "" && keep[n.Parent] {
			kept = append(kept, n)
		}
	}

	// 4. Exit reachability.
	for _, h := range stuckHeaders(l.edges, kept) {
		l.edges[CfgEdge{Src: h, Dst: exitTarget, Kind: EdgeException}] = true
	}

	// 5. Dedup and sort.
	return kept, l.sorted()
}

// registrations maps each deferred call expression in body to the key of its
// defer statement. Function literal bodies are not entered.
func registrations(body *ast.BlockStmt, keyOf map[ast.Node]string) map[ast.Node]string {
	out := map[ast.Node]string{}
	ast.Inspect(body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.DeferStmt:
			out[n.Call] = keyOf[n]
		}
		return true
	})
	return out
}

// successors returns the adjacency of edges, each list sorted, so walks are
// deterministic.
func successors(edges map[CfgEdge]bool) map[string][]string {
	succ := map[string][]string{}
	seen := map[[2]string]bool{}
	for e := range edges {
		if p := [2]string{e.Src, e.Dst}; !seen[p] {
			seen[p] = true
			succ[e.Src] = append(succ[e.Src], e.Dst)
		}
	}
	for _, s := range succ {
		sort.Strings(s)
	}
	return succ
}

// reachable returns the keys reachable from src, src included.
func reachable(edges map[CfgEdge]bool, src string) map[string]bool {
	succ := successors(edges)
	seen := map[string]bool{src: true}
	stack := []string{src}
	for len(stack) > 0 {
		x := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, y := range succ[x] {
			if !seen[y] {
				seen[y] = true
				stack = append(stack, y)
			}
		}
	}
	return seen
}

// stuckHeaders returns, for each bottom strongly connected component among
// the CFG nodes that cannot reach @exit, its header: the member first visited
// by a depth-first walk from @entry. One exit edge per bottom component makes
// every stuck node reach @exit (each one reaches some bottom component). The
// loop is found structurally, never by edge kind.
func stuckHeaders(edges map[CfgEdge]bool, kept []Node) []string {
	pred := map[string][]string{}
	for e := range edges {
		pred[e.Dst] = append(pred[e.Dst], e.Src)
	}
	reachesExit := map[string]bool{ExitKey: true}
	stack := []string{ExitKey}
	for len(stack) > 0 {
		x := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, p := range pred[x] {
			if !reachesExit[p] {
				reachesExit[p] = true
				stack = append(stack, p)
			}
		}
	}
	stuck := map[string]bool{}
	for _, n := range kept {
		if n.Parent == "" && !reachesExit[n.Key] {
			stuck[n.Key] = true
		}
	}
	if len(stuck) == 0 {
		return nil
	}

	succ := successors(edges)
	// Depth-first preorder from @entry: the header is the first member seen.
	order := map[string]int{}
	var dfs func(string)
	dfs = func(x string) {
		order[x] = len(order)
		for _, y := range succ[x] {
			if _, ok := order[y]; !ok {
				dfs(y)
			}
		}
	}
	dfs(EntryKey)

	// Tarjan's SCCs over the stuck subgraph (closed under successors).
	index, low, onStack := map[string]int{}, map[string]int{}, map[string]bool{}
	var st []string
	var headers []string
	var strong func(string)
	strong = func(v string) {
		index[v], low[v] = len(index), len(index)
		st = append(st, v)
		onStack[v] = true
		for _, w := range succ[v] {
			if _, ok := index[w]; !ok {
				strong(w)
				low[v] = min(low[v], low[w])
			} else if onStack[w] {
				low[v] = min(low[v], index[w])
			}
		}
		if low[v] != index[v] {
			return
		}
		var comp []string
		for {
			w := st[len(st)-1]
			st = st[:len(st)-1]
			onStack[w] = false
			comp = append(comp, w)
			if w == v {
				break
			}
		}
		in := map[string]bool{}
		for _, c := range comp {
			in[c] = true
		}
		h := comp[0]
		for _, c := range comp {
			for _, y := range succ[c] {
				if !in[y] {
					return // not a bottom component
				}
			}
			if order[c] < order[h] {
				h = c
			}
		}
		headers = append(headers, h)
	}
	keys := make([]string, 0, len(stuck))
	for k := range stuck {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return order[keys[i]] < order[keys[j]] })
	for _, k := range keys {
		if _, ok := index[k]; !ok {
			strong(k)
		}
	}
	return headers
}

// isPanicStmt reports whether s is a statement calling the builtin panic.
// The name may be shadowed, so the callee is resolved through info; without
// info the name decides.
func isPanicStmt(s ast.Node, info *types.Info) bool {
	es, ok := s.(*ast.ExprStmt)
	if !ok {
		return false
	}
	call, ok := ast.Unparen(es.X).(*ast.CallExpr)
	if !ok {
		return false
	}
	id, ok := ast.Unparen(call.Fun).(*ast.Ident)
	if !ok {
		return false
	}
	if info == nil {
		return id.Name == "panic"
	}
	b, ok := info.Uses[id].(*types.Builtin)
	return ok && b.Name() == "panic"
}

// canPanic reports whether body node n (a node's Stmt) can panic (spec
// §4.3.2). It inspects only the node's own expressions:
//   - a header: if Init/Cond; for Init/Cond/Post; range Key/Value/X; switch
//     Init/Tag; type switch Init/Assign; a select header has none;
//   - a clause head: a case's List, a comm clause's Comm;
//   - a defer / go statement: the call's function value (its receiver) and
//     arguments, evaluated at the statement — not the call, which runs at
//     exit or in the new goroutine;
//   - any other node (a simple statement, a return, a deferred call node's
//     *ast.CallExpr): the whole node.
//
// Nested statement bodies and function literals are never entered. Within
// those expressions it is true for: a call that is not a type conversion
// (builtins included, close among them); an index or slice expression (a
// generic instantiation is not one); a type assertion other than x.(type)
// and the comma-ok form; a pointer dereference; a selector through a
// pointer; a channel send; a / or % (or /=, %=) with a non-constant divisor.
// With info nil, every call, selector and star expression counts.
func canPanic(n ast.Node, info *types.Info) bool {
	var parts []ast.Node
	switch s := n.(type) {
	case *ast.IfStmt:
		parts = []ast.Node{s.Init, s.Cond}
	case *ast.ForStmt:
		parts = []ast.Node{s.Init, s.Cond, s.Post}
	case *ast.RangeStmt:
		parts = []ast.Node{s.Key, s.Value, s.X}
	case *ast.SwitchStmt:
		parts = []ast.Node{s.Init, s.Tag}
	case *ast.TypeSwitchStmt:
		parts = []ast.Node{s.Init, s.Assign}
	case *ast.SelectStmt, *ast.BlockStmt, *ast.LabeledStmt:
		return false
	case *ast.CaseClause:
		for _, x := range s.List {
			parts = append(parts, x)
		}
	case *ast.CommClause:
		parts = []ast.Node{s.Comm}
	case *ast.DeferStmt:
		parts = callOperands(s.Call)
	case *ast.GoStmt:
		parts = callOperands(s.Call)
	default:
		parts = []ast.Node{n}
	}
	for _, p := range parts {
		if p != nil && exprPanics(p, info) {
			return true
		}
	}
	return false
}

// callOperands is what a defer / go statement evaluates: the function value
// and the arguments.
func callOperands(call *ast.CallExpr) []ast.Node {
	out := []ast.Node{call.Fun}
	for _, a := range call.Args {
		out = append(out, a)
	}
	return out
}

// exprPanics walks n (no function literal bodies) for a panic-capable
// operation.
func exprPanics(n ast.Node, info *types.Info) bool {
	commaOK := map[ast.Expr]bool{}
	found := false
	ast.Inspect(n, func(x ast.Node) bool {
		if found {
			return false
		}
		switch x := x.(type) {
		case *ast.FuncLit:
			return false
		case *ast.AssignStmt:
			if len(x.Lhs) == 2 && len(x.Rhs) == 1 {
				commaOK[ast.Unparen(x.Rhs[0])] = true
			}
			if (x.Tok == token.QUO_ASSIGN || x.Tok == token.REM_ASSIGN) && !isConst(x.Rhs[0], info) {
				found = true
			}
		case *ast.ValueSpec:
			if len(x.Names) == 2 && len(x.Values) == 1 {
				commaOK[ast.Unparen(x.Values[0])] = true
			}
		case *ast.CallExpr:
			found = !isType(x.Fun, info)
		case *ast.IndexExpr:
			found = !isType(x.Index, info)
		case *ast.SliceExpr, *ast.SendStmt:
			found = true
		case *ast.TypeAssertExpr:
			found = x.Type != nil && !commaOK[x]
		case *ast.StarExpr:
			found = !isType(x, info)
		case *ast.SelectorExpr:
			found = viaPointer(x, info)
		case *ast.BinaryExpr:
			found = (x.Op == token.QUO || x.Op == token.REM) && !isConst(x.Y, info)
		}
		return !found
	})
	return found
}

// isType reports whether e denotes a type (false without info).
func isType(e ast.Expr, info *types.Info) bool {
	if info == nil {
		return false
	}
	tv, ok := info.Types[e]
	return ok && tv.IsType()
}

// isConst reports whether e is a constant expression. Without info only a
// basic literal is.
func isConst(e ast.Expr, info *types.Info) bool {
	if info == nil {
		_, ok := ast.Unparen(e).(*ast.BasicLit)
		return ok
	}
	tv, ok := info.Types[e]
	return ok && tv.Value != nil
}

// viaPointer reports whether selector x reads through a pointer: its
// receiver is pointer-typed, or the selection walks an embedded pointer. A
// qualified identifier (pkg.Name) and a method expression do not. Without
// info every selector does.
func viaPointer(x *ast.SelectorExpr, info *types.Info) bool {
	if info == nil {
		return true
	}
	sel, ok := info.Selections[x]
	if !ok || sel.Kind() == types.MethodExpr {
		return false
	}
	_, ptr := sel.Recv().Underlying().(*types.Pointer)
	return ptr || sel.Indirect()
}
