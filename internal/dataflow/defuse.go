package dataflow

import (
	"go/ast"
	"go/token"
	"go/types"
	"sort"
)

// DefUse is the def/use summary of one body node (spec §4.5).
//
//   - Defs are strong definitions: a reaching def d with p.Covers(d) for a p
//     in Defs is killed (`x = …` kills x, x.f, x[*]; `x.f = …` kills x.f…).
//   - WeakDefs are may-definitions: they reach on, but kill nothing (`m[k] =
//     v`, `*p = v`, a k-truncated write, what a callee may write through a
//     reference argument, the `<defer>:` pseudo path).
//   - Uses are the paths the node reads.
//
// A strong def never has a wildcard step (any write through an index, a
// dereference or past k is weak), so a kill never removes more than the
// written path covers. The `<defer>:` pseudo path (nil Base) appears only as
// a registration's weak def and its deferred call node's use. Each list is
// deduplicated and sorted by (Spell, base position).
type DefUse struct {
	Defs, WeakDefs, Uses []Path
}

// DeferPrefix starts the pseudo path a defer registration weak-defines and its
// deferred call node uses: "<defer>:" + the deferred call node's Key.
const DeferPrefix = "<defer>:"

// DefUseOf returns the def/use sets of body node n of callable fn (a
// *ast.FuncDecl or *ast.FuncLit). k is the access-path limit (PathOf).
// entryBases is EntryBases(fn, info), what @entry defines. keyOf is
// BuildNodes' map, used to find the deferred call node's key from a defer
// registration (keyOf[DeferStmt.Call]). info must be non-nil.
//
// Rules by node:
//
//   - @entry defines a base path for every object in entryBases; @exit has
//     none.
//   - A satellite call node (Parent != "") has an EMPTY DefUse. Its uses and
//     weak defs belong to its host statement (the Parent), whose DefUse is
//     computed from the host's whole expression, which contains the call —
//     so a flow is counted once, at the node on the CFG. Task 10 relies on
//     this: satellites are never CFG nodes and need no gen/kill.
//   - A header or clause head reads only its own header fields, never its
//     body (R8): if Init+Cond; for Init+Cond+Post; range Key/Value (defs) and
//     X (use); switch Init+Tag; type switch Init+Assign (defining each
//     clause's implicit variable); case List; comm clause Comm; select none.
//     A for header runs on entry and on every back edge, so only a variable
//     both Init and Post strong-define stays strong; other folded defs are
//     weak. Range Key/Value are weak with `=` (zero iterations leave them),
//     strong with `:=`. If and switch inits run once per header: strong.
//   - A bare call node (*ast.ExprStmt, `panic(x)` included) and a `go` node
//     (*ast.GoStmt) apply the call rule below to their call — for `go`, that
//     covers the calls nested in its arguments, which have no node of their
//     own.
//   - defer (spec §4.3.1). The registration node (*ast.DeferStmt) uses the
//     deferred call's function value / receiver and arguments (with the calls
//     nested in them) and weak-defines the pseudo path "<defer>:<call-key>",
//     <call-key> = keyOf[DeferStmt.Call], the deferred call node's Key. The
//     deferred call node (Deferred) uses that pseudo path, every named result
//     of fn and, for `defer func(){…}()`, what the closure captures; it
//     weak-defines what the call may write through (call rule) and never
//     strong-defines anything (R11: chain membership is unconditional).
//   - Assignment: each LHS is a def (`:=` reuses an existing object for an
//     already-declared name); `_` is none. An ident or a field path is a
//     strong def; a path through an index, a dereference or past k is weak.
//     Locating the target reads its index expressions and the pointer /
//     map / slice it goes through (`m[k] = v` uses k, m; `*q = v` and
//     `p.f = v` with p a pointer use q / p). Compound assignment and ++/--
//     also use the target. `var x, y = a, b` defines x, y.
//   - `ch <- v` weak-defines ch[*] and uses ch, v. A receive (`<-ch`, a
//     range over a channel) uses ch and ch[*].
//   - return uses its operands. With named results, a bare return uses them
//     and a return with operands defines them.
//
// Expression uses: a path is used as a whole (m[k] uses m[*] and k); a
// function literal uses every variable it captures, never its body. The call
// rule: a call uses its function value / receiver and arguments; it
// weak-defines a.* for each argument of pointer, slice, map, chan, func or
// interface type (`&x` gives x.*) and recv.* for a receiver that is
// reference-typed (the same kinds) or bound to a pointer-receiver method. A value-typed struct
// argument is not weak-defined. Conversions write nothing; builtins write
// nothing except copy(dst, …), delete(m, …) and clear(x), which weak-define
// dst[*], m[*], x[*]; append's result flows to the assignment's LHS.
func DefUseOf(n Node, info *types.Info, k int, entryBases []types.Object, fn ast.Node, keyOf map[ast.Node]string) DefUse {
	c := &collector{info: info, k: k, seen: map[pathKind]bool{}}
	switch {
	case n.Kind == KindEntry:
		for _, b := range entryBases {
			c.add(kindDef, Path{Base: b, Spell: baseSpell(b)})
		}
	case n.Kind == KindExit, n.Parent != "", info == nil:
	case n.Deferred:
		call, ok := n.Stmt.(*ast.CallExpr)
		if !ok {
			break
		}
		c.add(kindUse, Path{Spell: DeferPrefix + n.Key})
		for _, r := range namedResults(fn, info) {
			c.add(kindUse, Path{Base: r, Spell: r.Name()})
		}
		if lit, ok := ast.Unparen(call.Fun).(*ast.FuncLit); ok {
			c.captures(lit)
		}
		c.callWrites(call)
	default:
		c.fn = fn
		c.keyOf = keyOf
		c.stmt(n.Stmt)
	}
	sortPaths(c.du.Defs)
	sortPaths(c.du.WeakDefs)
	sortPaths(c.du.Uses)
	return c.du
}

type pathKind struct {
	p    Path
	kind int
}

const (
	kindDef = iota
	kindWeak
	kindUse
)

type collector struct {
	info     *types.Info
	k        int
	fn       ast.Node
	keyOf    map[ast.Node]string
	du       DefUse
	seen     map[pathKind]bool
	weakOnly bool // record every def as weak (a range with `=`)
}

// fork returns an empty collector with c's settings.
func (c *collector) fork() *collector {
	return &collector{info: c.info, k: c.k, fn: c.fn, keyOf: c.keyOf, seen: map[pathKind]bool{}}
}

// mergeLoop adds a's facts to c, keeping a strong def strong only when other
// strong-defines the same path too.
func (c *collector) mergeLoop(a, other *collector) {
	for _, p := range a.du.Defs {
		if other.seen[pathKind{p, kindDef}] {
			c.add(kindDef, p)
		} else {
			c.add(kindWeak, p)
		}
	}
	for _, p := range a.du.WeakDefs {
		c.add(kindWeak, p)
	}
	for _, p := range a.du.Uses {
		c.add(kindUse, p)
	}
}

// sortPaths orders ps by spelling, then base position (shadowed variables
// share a spelling), so DefUse lists are sorted, not just deterministic.
func sortPaths(ps []Path) {
	pos := func(p Path) token.Pos {
		if p.Base == nil {
			return token.NoPos
		}
		return p.Base.Pos()
	}
	sort.SliceStable(ps, func(i, j int) bool {
		if ps[i].Spell != ps[j].Spell {
			return ps[i].Spell < ps[j].Spell
		}
		return pos(ps[i]) < pos(ps[j])
	})
}

func (c *collector) add(kind int, p Path) {
	if c.seen[pathKind{p, kind}] {
		return
	}
	c.seen[pathKind{p, kind}] = true
	switch kind {
	case kindDef:
		c.du.Defs = append(c.du.Defs, p)
	case kindWeak:
		c.du.WeakDefs = append(c.du.WeakDefs, p)
	default:
		c.du.Uses = append(c.du.Uses, p)
	}
}

func (c *collector) addUse(b pathB)  { c.add(kindUse, b.limit(c.k)) }
func (c *collector) addWeak(b pathB) { c.add(kindWeak, b.limit(c.k)) }

// stmt collects a node's statement (or clause head) — header fields only.
func (c *collector) stmt(s ast.Node) {
	switch s := s.(type) {
	case *ast.AssignStmt:
		compound := s.Tok != token.ASSIGN && s.Tok != token.DEFINE
		for _, r := range s.Rhs {
			c.use(r)
		}
		for _, l := range s.Lhs {
			c.assignTo(l, compound)
		}
	case *ast.IncDecStmt:
		c.assignTo(s.X, true)
	case *ast.DeclStmt:
		gd, ok := s.Decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			return
		}
		for _, sp := range gd.Specs {
			vs := sp.(*ast.ValueSpec)
			for _, v := range vs.Values {
				c.use(v)
			}
			for _, name := range vs.Names {
				c.assignTo(name, false)
			}
		}
	case *ast.SendStmt:
		c.use(s.Value)
		if b, ok := pathOf(s.Chan, c.info); ok {
			c.addUse(b)
			c.indexReads(s.Chan)
			c.addWeak(b.with("[*]"))
		} else {
			c.use(s.Chan)
		}
	case *ast.ExprStmt:
		c.use(s.X)
	case *ast.CallExpr:
		c.call(s) // not produced by BuildNodes off a satellite; kept total
	case *ast.GoStmt:
		c.call(s.Call)
	case *ast.DeferStmt:
		c.callReads(s.Call, false)
		if key, ok := c.keyOf[s.Call]; ok {
			c.add(kindWeak, Path{Spell: DeferPrefix + key})
		}
	case *ast.ReturnStmt:
		for _, r := range s.Results {
			c.use(r)
		}
		kind := kindDef
		if len(s.Results) == 0 {
			kind = kindUse
		}
		for _, r := range namedResults(c.fn, c.info) {
			c.add(kind, Path{Base: r, Spell: r.Name()})
		}
	case *ast.IfStmt:
		c.stmt(s.Init)
		c.use(s.Cond)
	case *ast.ForStmt:
		// The header node runs on loop entry (where Init runs) and on every
		// back edge (where Post runs), so a strong def here would kill on the
		// path where its statement does not run: only a variable both Init
		// and Post define is strong; every other folded def is weak.
		init, post := c.fork(), c.fork()
		init.stmt(s.Init)
		post.stmt(s.Post)
		c.use(s.Cond)
		c.mergeLoop(init, post)
		c.mergeLoop(post, init)
	case *ast.RangeStmt:
		// `for k, v = range x` may run zero times, and then leaves k, v
		// untouched: weak. `:=` declares fresh objects per loop: strong.
		c.weakOnly = s.Tok == token.ASSIGN
		for _, x := range []ast.Expr{s.Key, s.Value} {
			if x != nil {
				c.assignTo(x, false)
			}
		}
		c.weakOnly = false
		if isChan(c.info.TypeOf(s.X)) {
			c.recv(s.X)
		} else {
			c.use(s.X)
		}
	case *ast.SwitchStmt:
		c.stmt(s.Init)
		c.use(s.Tag)
	case *ast.TypeSwitchStmt:
		c.stmt(s.Init)
		c.stmt(s.Assign) // `t := v.(type)`: t has no object, only v is read
		for _, cl := range s.Body.List {
			if obj := c.info.Implicits[cl]; obj != nil && obj.Name() != "_" {
				c.add(kindDef, Path{Base: obj, Spell: obj.Name()})
			}
		}
	case *ast.CaseClause:
		for _, x := range s.List {
			c.use(x)
		}
	case *ast.CommClause:
		c.stmt(s.Comm)
	}
	// SelectStmt, BranchStmt, EmptyStmt, nil: nothing.
}

// assignTo records lhs as an assignment target.
func (c *collector) assignTo(lhs ast.Expr, compound bool) {
	b, ok := pathOf(lhs, c.info)
	if !ok {
		c.use(lhs) // `_`, or a target rooted in a call: only its reads
		return
	}
	c.targetReads(ast.Unparen(lhs))
	if compound {
		c.addUse(b)
	}
	if c.weakOnly || b.weak(c.k) {
		c.addWeak(b)
	} else {
		c.add(kindDef, b.limit(c.k))
	}
}

// targetReads collects what locating assignment target e (a path) reads: its
// index expressions, and the pointer / map / slice value it goes through.
func (c *collector) targetReads(e ast.Expr) {
	switch e := e.(type) {
	case *ast.SelectorExpr:
		if isQualified(e, c.info) {
			return
		}
		x := ast.Unparen(e.X)
		if st, ok := x.(*ast.StarExpr); ok {
			c.use(st.X)
		} else if sel := c.info.Selections[e]; sel != nil && sel.Indirect() {
			c.use(x)
		} else {
			c.targetReads(x)
		}
	case *ast.IndexExpr:
		c.use(e.Index)
		x := ast.Unparen(e.X)
		if st, ok := x.(*ast.StarExpr); ok {
			c.use(st.X)
		} else if _, arr := underlying(c.info.TypeOf(x)).(*types.Array); arr {
			c.targetReads(x)
		} else {
			c.use(x)
		}
	case *ast.StarExpr:
		c.use(e.X)
	case *ast.ParenExpr:
		c.targetReads(ast.Unparen(e))
	}
}

// use collects the reads of expression e.
func (c *collector) use(e ast.Expr) {
	if e == nil {
		return
	}
	e = ast.Unparen(e)
	if b, ok := pathOf(e, c.info); ok {
		c.addUse(b)
		c.indexReads(e)
		return
	}
	switch e := e.(type) {
	case *ast.CallExpr:
		c.call(e)
	case *ast.FuncLit:
		c.captures(e)
	case *ast.UnaryExpr:
		if e.Op == token.ARROW {
			c.recv(e.X)
		} else {
			c.use(e.X)
		}
	case *ast.SelectorExpr:
		if !isQualified(e, c.info) {
			c.use(e.X) // a method value reads its receiver
		}
	default:
		ast.Inspect(e, func(n ast.Node) bool {
			if n == ast.Node(e) {
				return true
			}
			if x, ok := n.(ast.Expr); ok {
				c.use(x)
				return false
			}
			return true
		})
	}
}

// indexReads collects the index expressions inside path expression e.
func (c *collector) indexReads(e ast.Expr) {
	switch e := ast.Unparen(e).(type) {
	case *ast.SelectorExpr:
		if !isQualified(e, c.info) {
			c.indexReads(e.X)
		}
	case *ast.IndexExpr:
		c.use(e.Index)
		c.indexReads(e.X)
	case *ast.StarExpr:
		c.indexReads(e.X)
	}
}

// recv collects a receive from channel expression ch: ch and ch[*].
func (c *collector) recv(ch ast.Expr) {
	b, ok := pathOf(ch, c.info)
	if !ok {
		c.use(ch)
		return
	}
	c.addUse(b)
	c.addUse(b.with("[*]"))
	c.indexReads(ch)
}

// captures uses every variable function literal lit captures.
func (c *collector) captures(lit *ast.FuncLit) {
	for _, v := range freeVars(lit, c.info) {
		c.add(kindUse, Path{Base: v, Spell: v.Name()})
	}
}

// isQualified reports whether e is a qualified identifier pkg.Name.
func isQualified(e *ast.SelectorExpr, info *types.Info) bool {
	x, ok := e.X.(*ast.Ident)
	if !ok {
		return false
	}
	_, ok = info.Uses[x].(*types.PkgName)
	return ok
}

func underlying(t types.Type) types.Type {
	if t == nil {
		return nil
	}
	return t.Underlying()
}

func isChan(t types.Type) bool {
	_, ok := underlying(t).(*types.Chan)
	return ok
}
