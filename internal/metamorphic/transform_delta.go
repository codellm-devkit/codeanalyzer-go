package metamorphic

// The expected-delta transforms (MR8–MR12), the precision transform (MR13),
// the call-routing transform (MR14) and extract-then-inline (MR15).
//
// The delta transforms work only where the analysis is exact (brief
// amendment 2026-10-27). Their sites are statements over local scalar
// variables (basic types, never captured or address-taken) built from
// identifiers, literals and operators that cannot panic. The callable must
// have an L3 result and no defer, goto or label. Loops, select / switch /
// fallthrough, panic edges and the defer chain are out of scope: a site's
// own CFG edges are never exception or loop_back edges. Each delta is
// computed from the original's own edges (the pre view), so the relation
// states what changes, not what the edges are.
//
// Each callable gets at most one site, so deltas never interact. Sites are
// chosen with the transform's seeded generator.

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// ─── pre-view queries ─────────────────────────────────────────────────────────

type edgeRec struct{ kind, src, dst, label, prov string }

func (e edgeRec) dedge() DEdge {
	return DEdge{Kind: e.kind, Src: Pre(e.src), Dst: Pre(e.dst), Label: e.label, Prov: e.prov}
}

func edgesOf(cv *CV, kind string) []edgeRec {
	var out []edgeRec
	for _, e := range slices.Sorted(maps.Keys(cv.Edges)) {
		f := strings.Split(e, "|")
		if f[0] != kind {
			continue
		}
		r := edgeRec{kind: f[0], src: f[1], dst: f[2], label: f[3]}
		if len(f) > 4 {
			r.prov = f[4]
		}
		out = append(out, r)
	}
	return out
}

func filter(es []edgeRec, pred func(edgeRec) bool) []edgeRec {
	var out []edgeRec
	for _, e := range es {
		if pred(e) {
			out = append(out, e)
		}
	}
	return out
}

func into(cv *CV, kind, dst string) []edgeRec {
	return filter(edgesOf(cv, kind), func(e edgeRec) bool { return e.dst == dst })
}

func outOf(cv *CV, kind, src string) []edgeRec {
	return filter(edgesOf(cv, kind), func(e edgeRec) bool { return e.src == src })
}

// cfgReach is the set of CFG nodes reachable from src in cv.
func cfgReach(cv *CV, src string) map[string]bool {
	succ := map[string][]string{}
	for _, e := range edgesOf(cv, "cfg") {
		succ[e.src] = append(succ[e.src], e.dst)
	}
	seen := map[string]bool{src: true}
	stack := []string{src}
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, d := range succ[n] {
			if !seen[d] {
				seen[d] = true
				stack = append(stack, d)
			}
		}
	}
	return seen
}

// onCycle reports whether CFG node k reaches itself.
func onCycle(cv *CV, k string) bool {
	for _, e := range outOf(cv, "cfg", k) {
		if cfgReach(cv, e.dst)[k] {
			return true
		}
	}
	return false
}

// sameCDG reports whether cv's cdg equals the reference CDG of its cfg.
func sameCDG(cv *CV) bool {
	return cdgDiff(cv, RefCDG(cv)) == ""
}

// ─── eligibility ──────────────────────────────────────────────────────────────

// scope is the per-callable state of a delta transform.
type scope struct {
	f      *SrcFile
	fd     *ast.FuncDecl
	id     string
	cv     *CV
	pre    *View
	pinned map[*types.Var]bool
}

// deltaScope returns fd's scope, or why it is not eligible.
func deltaScope(c *tctx, f *SrcFile, fd *ast.FuncDecl) (*scope, string) {
	if fd.Body == nil {
		return nil, "no body"
	}
	if fd.Type.TypeParams != nil || recvGeneric(fd) {
		return nil, "generic"
	}
	if contains(fd.Body, func(n ast.Node) bool {
		switch n.(type) {
		case *ast.DeferStmt, *ast.LabeledStmt:
			return true
		}
		return false
	}) || hasGoto(fd.Body) {
		return nil, "defer, goto or label"
	}
	id := funcIdentity(f, fd)
	cv := c.pre.Calls[id]
	if cv == nil || !cv.L3 {
		return nil, "no L3 result"
	}
	if !sameCDG(cv) {
		// Its CDG is not the reference CDG of its CFG: a finding the test
		// reports separately (TestRefCDG), not a site.
		return nil, "cdg differs from the reference"
	}
	return &scope{f: f, fd: fd, id: id, cv: cv, pre: c.pre, pinned: pinnedVars(f, fd.Body)}, ""
}

// scalar is obj as a local scalar of s: a function-local variable of basic
// type, neither captured nor address-taken.
func (s *scope) scalar(obj types.Object) (*types.Var, bool) {
	v, ok := localVar(s.f, obj)
	if !ok || s.pinned[v] {
		return nil, false
	}
	b, ok := v.Type().Underlying().(*types.Basic)
	if !ok || b.Info()&(types.IsNumeric|types.IsString|types.IsBoolean) == 0 {
		return nil, false
	}
	return v, true
}

func (s *scope) obj(id *ast.Ident) types.Object {
	info := s.f.Pkg.TypesInfo
	if o := info.Defs[id]; o != nil {
		return o
	}
	return info.Uses[id]
}

// safeExpr reports whether e cannot panic and names only local scalars and
// constants: identifiers, literals, parentheses and operators other than
// division, remainder and shifts.
func (s *scope) safeExpr(e ast.Expr) bool {
	switch e := e.(type) {
	case *ast.BasicLit:
		return true
	case *ast.Ident:
		switch o := s.f.Pkg.TypesInfo.Uses[e].(type) {
		case *types.Const, *types.Nil:
			return true
		case *types.Var:
			_, ok := s.scalar(o)
			return ok
		}
		return false
	case *ast.ParenExpr:
		return s.safeExpr(e.X)
	case *ast.UnaryExpr:
		return (e.Op == token.SUB || e.Op == token.ADD || e.Op == token.NOT || e.Op == token.XOR) && s.safeExpr(e.X)
	case *ast.BinaryExpr:
		switch e.Op {
		case token.QUO, token.REM, token.SHL, token.SHR:
			return false
		}
		return s.safeExpr(e.X) && s.safeExpr(e.Y)
	}
	return false
}

// facts are a safe statement's defined and used variables, by object.
type facts struct {
	defs, uses []*types.Var
	names      map[string]bool // every identifier it names
	define     bool            // a := statement
}

func addVar(list []*types.Var, v *types.Var) []*types.Var {
	if slices.Contains(list, v) {
		return list
	}
	return append(list, v)
}

// safeStmt returns st's facts when st is a safe simple statement: an
// assignment (=, :=, or an operator assignment that cannot panic) of safe
// expressions to local scalars, or ++/-- of one.
func (s *scope) safeStmt(st ast.Stmt) (*facts, bool) {
	fs := &facts{names: map[string]bool{}}
	ast.Inspect(st, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok {
			fs.names[id.Name] = true
		}
		return true
	})
	usesOf := func(e ast.Expr) {
		ast.Inspect(e, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok {
				if v, ok := s.scalar(s.f.Pkg.TypesInfo.Uses[id]); ok {
					fs.uses = addVar(fs.uses, v)
				}
			}
			return true
		})
	}
	lhs := func(e ast.Expr, use bool) bool {
		id, ok := e.(*ast.Ident)
		if !ok || id.Name == "_" {
			return false
		}
		v, ok := s.scalar(s.obj(id))
		if !ok {
			return false
		}
		fs.defs = addVar(fs.defs, v)
		if use {
			fs.uses = addVar(fs.uses, v)
		}
		return true
	}
	switch st := st.(type) {
	case *ast.AssignStmt:
		switch st.Tok {
		case token.ASSIGN, token.DEFINE:
		case token.ADD_ASSIGN, token.SUB_ASSIGN, token.MUL_ASSIGN, token.AND_ASSIGN, token.OR_ASSIGN,
			token.XOR_ASSIGN, token.AND_NOT_ASSIGN:
		default:
			return nil, false
		}
		if len(st.Lhs) != len(st.Rhs) {
			return nil, false
		}
		op := st.Tok != token.ASSIGN && st.Tok != token.DEFINE
		for _, r := range st.Rhs {
			if !s.safeExpr(r) {
				return nil, false
			}
			usesOf(r)
		}
		for _, l := range st.Lhs {
			if !lhs(l, op) {
				return nil, false
			}
		}
		fs.define = st.Tok == token.DEFINE
	case *ast.IncDecStmt:
		if !lhs(st.X, true) {
			return nil, false
		}
	default:
		return nil, false
	}
	return fs, true
}

// safeReturn reports whether st is a return of safe expressions.
func (s *scope) safeReturn(st ast.Stmt) bool {
	r, ok := st.(*ast.ReturnStmt)
	if !ok {
		return false
	}
	for _, e := range r.Results {
		if !s.safeExpr(e) {
			return false
		}
	}
	return true
}

// resultNamed reports whether name is a named result of s's function.
func (s *scope) resultNamed(name string) bool {
	if s.fd.Type.Results == nil {
		return false
	}
	for _, f := range s.fd.Type.Results.List {
		for _, n := range f.Names {
			if n.Name == name {
				return true
			}
		}
	}
	return false
}

// key is the body key of statement st, which must be a CFG node of s.cv.
func (s *scope) key(st ast.Stmt) (string, bool) {
	k := s.f.key(st.Pos())
	b, ok := s.cv.Body[k]
	if !ok || strings.Split(b, "|")[1] != "" {
		return "", false
	}
	return k, true
}

// only reports whether k's only CFG out-edge is a fallthrough to a node
// other than @exit, and returns it.
func (s *scope) only(k string) (edgeRec, bool) {
	out := outOf(s.cv, "cfg", k)
	if len(out) != 1 || out[0].label != "fallthrough" || out[0].dst == "@exit" {
		return edgeRec{}, false
	}
	return out[0], true
}

func zeroLit(t types.Type) string {
	b := t.Underlying().(*types.Basic)
	switch {
	case b.Info()&types.IsString != 0:
		return `""`
	case b.Info()&types.IsBoolean != 0:
		return "false"
	}
	return "0"
}

// deltaTransform runs pick on every eligible function of the file, at most
// one site each.
func deltaTransform(name string, pick func(c *tctx, s *scope, fe *fileEdit) []edit) Transform {
	return Transform{Name: name, edit: func(c *tctx, f *SrcFile) (*fileEdit, error) {
		if c.pre == nil {
			return nil, fmt.Errorf("%s needs the original's analysis", name)
		}
		if usesPrefix(f, strings.ToLower(name)) {
			return nil, errSkip(name + ": name clash")
		}
		fe := &fileEdit{}
		var edits []edit
		for _, fd := range funcDecls(f) {
			s, why := deltaScope(c, f, fd)
			if s == nil {
				fe.skip(name + ": callable: " + why)
				continue
			}
			es := pick(c, s, fe)
			if len(es) == 0 {
				fe.skip(name + ": callable: no eligible site")
				continue
			}
			edits = append(edits, es...)
			fe.touched = append(fe.touched, s.id)
		}
		if len(edits) == 0 {
			return fe, errSkip(name + ": no eligible site")
		}
		ps, err := applyEdits(f, edits)
		fe.pieces = ps
		return fe, err
	}}
}

// choose returns one of n candidates with c's generator.
func (c *tctx) choose(n int) int { return c.rng.IntN(n) }

func desc(s *scope, st ast.Node, what string) string {
	return fmt.Sprintf("%s:%d: %s: %s", s.f.Rel, s.f.pos(st.Pos()).Line, s.fd.Name.Name, what)
}

// statements is every statement list of s's body outside function literals,
// with each statement.
func (s *scope) lists() [][]ast.Stmt {
	var out [][]ast.Stmt
	stmtLists(s.fd.Body, func(l []ast.Stmt) { out = append(out, l) })
	return out
}

// ─── delta builder ────────────────────────────────────────────────────────────

// dbuild accumulates a Delta over a callable's pre view. Its DDG helpers
// keep the L4 port mirror of an @entry edge in step: an L3 edge @entry → n
// on a base that has a formal_in is mirrored as @formal_in:<i> → n on the
// same var, prov reaching-defs (spec §4.7 port wiring).
type dbuild struct {
	d      *Delta
	cv     *CV
	called bool
}

func newDelta(s *scope, site string, nodes map[string]string) *dbuild {
	return &dbuild{d: &Delta{Site: site, Callable: s.id, Nodes: nodes}, cv: s.cv, called: s.pre.called(s.id)}
}

// formalIn is the @formal_in key whose `of` is name ("" when none).
func (b *dbuild) formalIn(name string) string {
	for k, attr := range b.cv.Body {
		if strings.HasPrefix(k, "@formal_in:") && strings.Split(attr, "|")[2] == name {
			return k
		}
	}
	return ""
}

func (b *dbuild) rm(e edgeRec) {
	b.d.Remove = append(b.d.Remove, e.dedge())
	if e.kind == "ddg" && e.src == "@entry" {
		if fi := b.formalIn(e.label); fi != "" && b.cv.Edges["ddg|"+fi+"|"+e.dst+"|"+e.label+"|reaching-defs"] {
			b.d.Remove = append(b.d.Remove, Port(Pre(fi), Pre(e.dst), e.label))
		}
	}
}

func (b *dbuild) add(e DEdge) {
	for _, a := range b.d.Add {
		if a == e {
			return
		}
	}
	b.d.Add = append(b.d.Add, e)
	if e.Kind == "ddg" && e.Src.Pre == "@entry" {
		if fi := b.formalIn(e.Label); fi != "" {
			b.add(Port(Pre(fi), e.Dst, e.Label))
		}
	}
}

// ssaOn are the ssa ddg edges of es on var v; ok is false when es holds a
// non-ssa edge on v other than an @formal_in port mirror (a site the delta
// rules do not describe).
func ssaOn(es []edgeRec, v string) ([]edgeRec, bool) {
	var out []edgeRec
	for _, e := range es {
		if e.label != v {
			continue
		}
		switch {
		case e.prov == "ssa":
			out = append(out, e)
		case e.prov == "reaching-defs" && strings.HasPrefix(e.src, "@formal_in:"):
		default:
			return nil, false
		}
	}
	return out, true
}

// sym is e with its refs spelled symbolically: a pre key as itself, a mark
// as "mark:<name>".
func (e DEdge) sym() string {
	ref := func(r Ref) string {
		if r.Mark != "" {
			return "mark:" + r.Mark
		}
		return r.Pre
	}
	out := e.Kind + "|" + ref(e.Src) + "|" + ref(e.Dst) + "|" + e.Label
	if e.Kind == "ddg" {
		out += "|" + cmpOr(e.Prov, "ssa")
	}
	return out
}

// localFlows is the set of (formal_in, formal_out) pairs of a callable's
// edges, the flows its summary is computed from: reachability over its ddg,
// cdg and summary edges (sdg graphOf).
func localFlows(edges map[string]bool) map[[2]string]bool {
	succ := map[string][]string{}
	var srcs []string
	for e := range edges {
		f := strings.Split(e, "|")
		if f[0] == "cfg" {
			continue
		}
		succ[f[1]] = append(succ[f[1]], f[2])
		if strings.HasPrefix(f[1], "@formal_in:") {
			srcs = append(srcs, f[1])
		}
	}
	out := map[[2]string]bool{}
	for _, s := range srcs {
		for n := range (Graph(succ)).Reach(s) {
			if strings.HasPrefix(n, "@formal_out") {
				out[[2]string{s, n}] = true
			}
		}
	}
	return out
}

// preserves reports whether the delta leaves the callable's flows as they
// are. A delta that changes them would change the summary edges of every
// caller, outside the callable, which the delta oracle does not predict.
//
// The predicted edges name new nodes by mark ("mark:<name>"); its CDG is
// derived from its CFG when the delta says so.
func (b *dbuild) preserves() bool {
	post := maps.Clone(b.cv.Edges)
	for _, e := range b.d.Remove {
		delete(post, e.sym())
	}
	for _, e := range b.d.Add {
		post[e.sym()] = true
	}
	if b.d.CDGFromCFG {
		for e := range post {
			if strings.HasPrefix(e, "cdg|") {
				delete(post, e)
			}
		}
		for e := range RefCDG(&CV{Edges: post}) {
			post[e] = true
		}
	}
	return maps.Equal(localFlows(b.cv.Edges), localFlows(post))
}

// done records the delta as a site of fe, or the reason it is not one.
//
// A callable no in-project call site targets (no param_in edge into it) may
// change its flows: no caller's summary edges depend on them.
func (b *dbuild) done(fe *fileEdit, name string) bool {
	if b.called && !b.preserves() {
		fe.skip(name + ": site: changes the callable's flows")
		return false
	}
	fe.sites = append(fe.sites, Site{Desc: b.d.Site, Callable: b.d.Callable, Delta: b.d})
	return true
}

// ─── MR8: wrap a statement in `if c { S }` ────────────────────────────────────

// MR8 wraps a statement S in `if c := true; c { S }`. S is a safe
// assignment that reads every variable it writes (x++, x += e, x = x + e),
// so every def reaching S is visible as one of its ddg in-edges, and it
// falls through to its successor Y. Expected delta:
//   - CFG: each X→S becomes X→B (same kind); B→S true; B→Y false.
//   - CDG: derived from the expected CFG (the reference CDG).
//   - DDG: S is now conditional, so each def D reaching S on x also reaches
//     each use U that S's def of x reached: D→U on x, unless already there
//     (with its @formal_in port mirror when D is @entry). The guard c is
//     defined by the header's init and used by its condition, both folded
//     into the header node B (spec §4.2). Within a node uses precede defs,
//     so c adds no edge in straight-line code; when S lies on a CFG cycle
//     (inside a loop body) the init's def reaches the condition around the
//     loop, and c adds the self-loop B→B (a sound over-approximation of the
//     header folding).
var MR8 = deltaTransform("MR8", func(c *tctx, s *scope, fe *fileEdit) []edit {
	type cand struct {
		st ast.Stmt
		k  string
		y  edgeRec
		fs *facts
	}
	var cands []cand
	for _, list := range s.lists() {
		for _, st := range list {
			fs, ok := s.safeStmt(st)
			if !ok || fs.define || slices.ContainsFunc(fs.defs, func(v *types.Var) bool { return !slices.Contains(fs.uses, v) }) {
				continue
			}
			k, ok := s.key(st)
			if !ok {
				continue
			}
			y, ok := s.only(k)
			if !ok {
				continue
			}
			cands = append(cands, cand{st, k, y, fs})
		}
	}
	for len(cands) > 0 {
		i := c.choose(len(cands))
		x := cands[i]
		cands = slices.Delete(cands, i, i+1)
		b, g := c.fresh("mr8b"), c.fresh("mr8c")
		db := newDelta(s, desc(s, x.st, "wrap in if"), map[string]string{b: "branch"})
		db.d.CDGFromCFG = true
		for _, e := range into(s.cv, "cfg", x.k) {
			db.rm(e)
			db.add(CFG(Pre(e.src), Mark(b), e.label))
		}
		db.add(CFG(Mark(b), Pre(x.k), "true"))
		db.add(CFG(Mark(b), Pre(x.y.dst), "false"))
		if onCycle(s.cv, x.k) {
			db.add(DDG(Mark(b), Mark(b), g))
		}
		ok := true
		for _, v := range x.fs.defs {
			ins, ok1 := ssaOn(into(s.cv, "ddg", x.k), v.Name())
			outs, ok2 := ssaOn(outOf(s.cv, "ddg", x.k), v.Name())
			if !ok1 || !ok2 {
				ok = false
				break
			}
			for _, in := range ins {
				for _, o := range outs {
					if !s.cv.Edges["ddg|"+in.src+"|"+o.dst+"|"+v.Name()+"|ssa"] {
						db.add(DDG(Pre(in.src), Pre(o.dst), v.Name()))
					}
				}
			}
		}
		if !ok {
			fe.skip("MR8: site: non-ssa ddg on the variable")
			continue
		}
		if !db.done(fe, "MR8") {
			continue
		}
		a, z := s.f.off(x.st.Pos()), s.f.off(x.st.End())
		return []edit{
			insert(a, mark(b), lit("if "+g+" := true; "+g+" { ")),
			insert(z, lit(" }")),
		}
	}
	return nil
})

// ─── MR9: insert a kill ───────────────────────────────────────────────────────

// MR9 inserts the kill `x = k` (k the zero literal of x's type) between a
// safe statement D that defines local scalar x and the next statement U,
// which D falls through to and which reads x (D→U on x). Every path from D
// passes the kill, so expected delta:
//   - DDG: each D→W on x becomes K→W on x.
//   - CFG: D→U becomes D→K, K→U (fallthrough).
//   - CDG: derived (K gets D's controllers).
var MR9 = deltaTransform("MR9", func(c *tctx, s *scope, fe *fileEdit) []edit {
	type cand struct {
		d, u   ast.Stmt
		dk, uk string
		v      *types.Var
	}
	var cands []cand
	for _, list := range s.lists() {
		for i := 0; i+1 < len(list); i++ {
			fs, ok := s.safeStmt(list[i])
			if !ok {
				continue
			}
			dk, ok1 := s.key(list[i])
			uk, ok2 := s.key(list[i+1])
			if !ok1 || !ok2 {
				continue
			}
			if y, ok := s.only(dk); !ok || y.dst != uk {
				continue
			}
			for _, v := range fs.defs {
				if s.cv.Edges["ddg|"+dk+"|"+uk+"|"+v.Name()+"|ssa"] {
					cands = append(cands, cand{list[i], list[i+1], dk, uk, v})
				}
			}
		}
	}
	for len(cands) > 0 {
		i := c.choose(len(cands))
		x := cands[i]
		cands = slices.Delete(cands, i, i+1)
		k := c.fresh("mr9k")
		name := x.v.Name()
		db := newDelta(s, desc(s, x.u, "kill "+name+" before"), map[string]string{k: "statement"})
		db.d.CDGFromCFG = true
		// Every def edge of D on x moves to the kill: the ssa edges to its
		// uses, and the reaching-defs port edge to the formal_out of a
		// named result x (the @exit rule, spec §4.7), whose var is the
		// formal_out's `of`.
		outs := filter(outOf(s.cv, "ddg", x.dk), func(e edgeRec) bool {
			return e.label == name || e.prov == "reaching-defs" && strings.HasPrefix(e.dst, "@formal_out") && s.resultNamed(name)
		})
		if slices.ContainsFunc(outs, func(e edgeRec) bool { return e.prov != "ssa" && !strings.HasPrefix(e.dst, "@formal_out") }) {
			fe.skip("MR9: site: non-ssa ddg on the variable")
			continue
		}
		for _, e := range outs {
			db.rm(e)
			db.add(DEdge{Kind: "ddg", Src: Mark(k), Dst: Pre(e.dst), Label: e.label, Prov: e.prov})
		}
		db.rm(edgeRec{kind: "cfg", src: x.dk, dst: x.uk, label: "fallthrough"})
		db.add(CFG(Pre(x.dk), Mark(k), "fallthrough"))
		db.add(CFG(Mark(k), Pre(x.uk), "fallthrough"))
		if !db.done(fe, "MR9") {
			continue
		}
		return []edit{insert(s.f.off(x.u.Pos()), mark(k), lit(name+" = "+zeroLit(x.v.Type())+"; "))}
	}
	return nil
})

// ─── MR10: insert a copy ──────────────────────────────────────────────────────

// MR10 inserts `y := x` before a statement U and rewrites U's one read of
// local scalar x to y. U is a safe statement or a safe return that names x
// only there. Expected delta:
//   - DDG: each D→U on x becomes D→C on x (with its port mirror when D is
//     @entry); C→U on y.
//   - CFG: each X→U becomes X→C (same kind); C→U fallthrough.
//   - CDG: derived (C gets U's controllers).
var MR10 = deltaTransform("MR10", func(c *tctx, s *scope, fe *fileEdit) []edit {
	if usesPrefix(s.f, "mr10y") {
		return nil
	}
	type cand struct {
		u  ast.Stmt
		uk string
		id *ast.Ident
		v  *types.Var
	}
	var cands []cand
	info := s.f.Pkg.TypesInfo
	for _, list := range s.lists() {
		for _, st := range list {
			fs, ok := s.safeStmt(st)
			if !ok && !s.safeReturn(st) {
				continue
			}
			uk, ok := s.key(st)
			if !ok {
				continue
			}
			byVar := map[*types.Var][]*ast.Ident{}
			ast.Inspect(st, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok {
					if v, ok := s.scalar(s.obj(id)); ok {
						byVar[v] = append(byVar[v], id)
					}
				}
				return true
			})
			for _, v := range slices.SortedFunc(maps.Keys(byVar), func(a, b *types.Var) int { return int(a.Pos() - b.Pos()) }) {
				ids := byVar[v]
				if len(ids) != 1 || info.Uses[ids[0]] != v {
					continue // named twice, or only written
				}
				if fs != nil && slices.Contains(fs.defs, v) {
					continue // an operator assignment reads and writes x
				}
				cands = append(cands, cand{st, uk, ids[0], v})
			}
		}
	}
	for len(cands) > 0 {
		i := c.choose(len(cands))
		x := cands[i]
		cands = slices.Delete(cands, i, i+1)
		name := x.v.Name()
		ins, ok := ssaOn(into(s.cv, "ddg", x.uk), name)
		if !ok || len(ins) == 0 {
			fe.skip("MR10: site: no ssa def of the variable reaches it")
			continue
		}
		cm, y := c.fresh("mr10c"), c.fresh("mr10y")
		db := newDelta(s, desc(s, x.u, "copy "+name+" to "+y), map[string]string{cm: "statement"})
		db.d.CDGFromCFG = true
		for _, e := range ins {
			db.rm(e)
			db.add(DDG(Pre(e.src), Mark(cm), name))
		}
		db.add(DDG(Mark(cm), Pre(x.uk), y))
		for _, e := range into(s.cv, "cfg", x.uk) {
			db.rm(e)
			db.add(CFG(Pre(e.src), Mark(cm), e.label))
		}
		db.add(CFG(Mark(cm), Pre(x.uk), "fallthrough"))
		if !db.done(fe, "MR10") {
			continue
		}
		return []edit{
			insert(s.f.off(x.u.Pos()), mark(cm), lit(y+" := "+name+"; ")),
			{a: s.f.off(x.id.Pos()), z: s.f.off(x.id.End()), anchor: true, out: []piece{lit(y)}},
		}
	}
	return nil
})

// ─── MR11: swap two independent statements ────────────────────────────────────

// MR11 swaps adjacent safe statements S1; S2 that share no variable by name
// (neither writes a name the other mentions, so a := cannot shadow). Their
// text moves, and their keys map to the new positions. Expected delta:
//   - DDG: none (after mapping).
//   - CFG: each X→S1 becomes X→S2; S1→S2 becomes S2→S1; S2's out-edges
//     leave S1 instead (same kinds).
//   - CDG: derived. It equals the original's, since S1 and S2 sit in one
//     basic block.
var MR11 = deltaTransform("MR11", func(c *tctx, s *scope, fe *fileEdit) []edit {
	type cand struct {
		s1, s2 ast.Stmt
		k1, k2 string
	}
	var cands []cand
	for _, list := range s.lists() {
		for i := 0; i+1 < len(list); i++ {
			f1, ok1 := s.safeStmt(list[i])
			f2, ok2 := s.safeStmt(list[i+1])
			if !ok1 || !ok2 {
				continue
			}
			if slices.ContainsFunc(f1.defs, func(v *types.Var) bool { return f2.names[v.Name()] }) ||
				slices.ContainsFunc(f2.defs, func(v *types.Var) bool { return f1.names[v.Name()] }) {
				continue
			}
			k1, ok1 := s.key(list[i])
			k2, ok2 := s.key(list[i+1])
			if !ok1 || !ok2 {
				continue
			}
			if y, ok := s.only(k1); !ok || y.dst != k2 {
				continue
			}
			if _, ok := s.only(k2); !ok {
				continue
			}
			cands = append(cands, cand{list[i], list[i+1], k1, k2})
		}
	}
	if len(cands) == 0 {
		return nil
	}
	x := cands[c.choose(len(cands))]
	db := newDelta(s, desc(s, x.s1, "swap with the next statement"), nil)
	db.d.CDGFromCFG = true
	for _, e := range into(s.cv, "cfg", x.k1) {
		db.rm(e)
		db.add(CFG(Pre(e.src), Pre(x.k2), e.label))
	}
	db.rm(edgeRec{kind: "cfg", src: x.k1, dst: x.k2, label: "fallthrough"})
	db.add(CFG(Pre(x.k2), Pre(x.k1), "fallthrough"))
	for _, e := range outOf(s.cv, "cfg", x.k2) {
		db.rm(e)
		db.add(CFG(Pre(x.k1), Pre(e.dst), e.label))
	}
	if !db.done(fe, "MR11") {
		return nil
	}
	f := s.f
	a1, z1 := f.off(x.s1.Pos()), f.off(x.s1.End())
	a2, z2 := f.off(x.s2.Pos()), f.off(x.s2.End())
	return []edit{{a: a1, z: z2, out: []piece{cp(f, a2, z2), cp(f, z1, a2), cp(f, a1, z1)}}}
})

// ─── MR12: an early return ────────────────────────────────────────────────────

// MR12 inserts `if c := true; c { return <zero values> }` before a safe
// statement S of the function's top-level statement list. The function's
// results are unnamed, so the return names no variable. Expected delta:
//   - CFG: each X→S becomes X→B (same kind); B→R true; R→@exit return;
//     B→S false.
//   - CDG: derived. Every node after S that @entry controlled is now
//     controlled by B.
//   - DDG: R → each @formal_out of a result (reaching-defs), the return's
//     port wiring. S is not in a loop, so the guard c adds no edge (see
//     MR8).
var MR12 = deltaTransform("MR12", func(c *tctx, s *scope, fe *fileEdit) []edit {
	for k, attr := range s.cv.Body {
		if strings.HasPrefix(k, "@formal_out") && !strings.HasPrefix(strings.Split(attr, "|")[2], "<return>") {
			return nil // a by-ref, capture or global out: an early exit adds def → exit paths
		}
	}
	res := s.fd.Type.Results
	var zeros []string
	if res != nil {
		for _, fl := range res.List {
			if len(fl.Names) > 0 {
				return nil
			}
			z, ok := zeroValue(s.f, s.f.Pkg.TypesInfo.TypeOf(fl.Type))
			if !ok {
				return nil
			}
			zeros = append(zeros, z)
		}
	}
	type cand struct {
		st ast.Stmt
		k  string
	}
	var cands []cand
	for _, st := range s.fd.Body.List {
		if _, ok := s.safeStmt(st); !ok && !s.safeReturn(st) {
			continue
		}
		if k, ok := s.key(st); ok {
			cands = append(cands, cand{st, k})
		}
	}
	for len(cands) > 0 {
		i := c.choose(len(cands))
		x := cands[i]
		cands = slices.Delete(cands, i, i+1)
		b, r, g := c.fresh("mr12b"), c.fresh("mr12r"), c.fresh("mr12c")
		db := newDelta(s, desc(s, x.st, "early return before"), map[string]string{b: "branch", r: "return"})
		db.d.CDGFromCFG = true
		for _, e := range into(s.cv, "cfg", x.k) {
			db.rm(e)
			db.add(CFG(Pre(e.src), Mark(b), e.label))
		}
		db.add(CFG(Mark(b), Mark(r), "true"))
		db.add(CFG(Mark(r), Pre("@exit"), "return"))
		db.add(CFG(Mark(b), Pre(x.k), "false"))
		for _, k := range slices.Sorted(maps.Keys(s.cv.Body)) {
			if of := strings.Split(s.cv.Body[k], "|")[2]; strings.HasPrefix(k, "@formal_out") && strings.HasPrefix(of, "<return>") {
				db.add(Port(Mark(r), Pre(k), of))
			}
		}
		if !db.done(fe, "MR12") {
			continue
		}
		ret := "return"
		if len(zeros) > 0 {
			ret += " " + strings.Join(zeros, ", ")
		}
		return []edit{insert(s.f.off(x.st.Pos()), mark(b), lit("if "+g+" := true; "+g+" { "), mark(r), lit(ret+" }; "))}
	}
	return nil
})

// zeroValue spells t's zero value in f, using no variable.
func zeroValue(f *SrcFile, t types.Type) (string, bool) {
	if t == nil {
		return "", false
	}
	if _, isTP := t.(*types.TypeParam); isTP {
		return "", false
	}
	switch u := t.Underlying().(type) {
	case *types.Basic:
		switch {
		case u.Info()&types.IsString != 0:
			return `""`, true
		case u.Info()&types.IsBoolean != 0:
			return "false", true
		case u.Info()&types.IsNumeric != 0:
			return "0", true
		case u.Kind() == types.UnsafePointer:
			return "nil", true
		}
		return "", false
	case *types.Pointer, *types.Slice, *types.Map, *types.Chan, *types.Signature, *types.Interface:
		return "nil", true
	case *types.Struct, *types.Array:
		s, ok := spell(f, t)
		if !ok {
			return "", false
		}
		return s + "{}", true
	}
	return "", false
}

// ─── MR13: a constant sink argument ───────────────────────────────────────────

// MR13 replaces the argument x of a call statement `g(…, x, …)` with the
// zero literal of x's type. x is a local scalar that the callable also names
// elsewhere, and every other argument is a constant. g is a plain function
// (no receiver, not variadic) whose formal_outs are only its results, and
// that touches no global, so the argument's value can only come from x.
// Before the change, some non-global @formal_in of the caller reaches the
// argument's actual_in over data edges.
//
// Expected (precision): afterwards no non-global @formal_in of the caller
// reaches that actual_in over data edges (ddg ∪ summary ∪ param_in ∪
// param_out). So "no flow" can happen.
// Nothing may be added anywhere, and nothing removed but the caller's ddg
// edges into the call on x and summary edges anywhere (they follow from the
// flows that went).
var MR13 = Transform{Name: "MR13", edit: func(c *tctx, f *SrcFile) (*fileEdit, error) {
	if c.pre == nil {
		return nil, fmt.Errorf("MR13 needs the original's analysis")
	}
	info := f.Pkg.TypesInfo
	fe := &fileEdit{}
	var edits []edit
	g := c.pre.Graph(true)
	for _, fd := range funcDecls(f) {
		if fd.Body == nil {
			continue
		}
		id := funcIdentity(f, fd)
		cv := c.pre.Calls[id]
		if cv == nil || !cv.L3 {
			fe.skip("MR13: callable: no L3 result")
			continue
		}
		s := &scope{f: f, fd: fd, id: id, cv: cv, pre: c.pre, pinned: pinnedVars(f, fd.Body)}
		uses := map[*types.Var]int{}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok {
				if v, ok := info.Uses[id].(*types.Var); ok {
					uses[v]++
				}
			}
			return true
		})
		var srcs []string
		for k, b := range cv.Body {
			if strings.HasPrefix(k, "@formal_in:") && !strings.HasPrefix(strings.Split(b, "|")[2], "<global>:") {
				srcs = append(srcs, Node(id, k))
			}
		}
		slices.Sort(srcs)
		var cands []mr13Cand
		for _, list := range s.lists() {
			for _, st := range list {
				x, ok := mr13Site(s, st, uses)
				if !ok {
					continue
				}
				if !g.ReachesFrom(srcs, Node(id, x.vk)) {
					fe.skip("MR13: site: no flow to the argument before")
					continue
				}
				cands = append(cands, x)
			}
		}
		if len(cands) == 0 {
			fe.skip("MR13: callable: no eligible site")
			continue
		}
		x := cands[c.choose(len(cands))]
		d := &Delta{Site: desc(s, x.arg, "constant argument for "+x.v.Name()), Callable: id, AtMost: true}
		for _, e := range filter(into(cv, "ddg", x.ck), func(e edgeRec) bool { return e.label == x.v.Name() }) {
			d.Remove = append(d.Remove, e.dedge())
		}
		fe.sites = append(fe.sites, Site{Desc: d.Site, Callable: id, Delta: d, NoFlow: []Ref{Pre(x.vk)}})
		fe.touched = append(fe.touched, id)
		edits = append(edits, edit{a: f.off(x.arg.Pos()), z: f.off(x.arg.End()), anchor: true, out: []piece{lit(zeroLit(x.v.Type()))}})
	}
	if len(edits) == 0 {
		return fe, errSkip("MR13: no eligible site")
	}
	ps, err := applyEdits(f, edits)
	fe.pieces = ps
	return fe, err
}}

type mr13Cand struct {
	arg    *ast.Ident
	v      *types.Var
	ck, vk string
}

// mr13Site is st as an MR13 site (see MR13).
func mr13Site(s *scope, st ast.Stmt, uses map[*types.Var]int) (mr13Cand, bool) {
	info := s.f.Pkg.TypesInfo
	es, ok := st.(*ast.ExprStmt)
	if !ok {
		return mr13Cand{}, false
	}
	call, ok := es.X.(*ast.CallExpr)
	if !ok || call.Ellipsis.IsValid() {
		return mr13Cand{}, false
	}
	fn, ok := ast.Unparen(call.Fun).(*ast.Ident)
	if !ok {
		return mr13Cand{}, false
	}
	callee, ok := info.Uses[fn].(*types.Func)
	if !ok {
		return mr13Cand{}, false
	}
	sig := callee.Type().(*types.Signature)
	if sig.Variadic() || sig.Recv() != nil || sig.TypeParams() != nil || sig.Params().Len() != len(call.Args) {
		return mr13Cand{}, false
	}
	ck := s.f.key(call.Pos())
	cb, ok := s.cv.Body[ck]
	if !ok {
		return mr13Cand{}, false
	}
	ccv := s.pre.Calls[strings.Split(cb, "|")[4]]
	if ccv == nil {
		return mr13Cand{}, false
	}
	for _, b := range ccv.Body {
		f := strings.Split(b, "|")
		if strings.HasPrefix(f[2], "<global>:") || f[0] == "formal_out" && !strings.HasPrefix(f[2], "<return>") {
			return mr13Cand{}, false
		}
	}
	var x mr13Cand
	for i, a := range call.Args {
		if tv, ok := info.Types[a]; ok && tv.Value != nil {
			continue
		}
		id, ok := a.(*ast.Ident)
		if !ok || x.arg != nil {
			return mr13Cand{}, false
		}
		v, ok := s.scalar(info.Uses[id])
		// A local must keep a use, or it no longer compiles; a parameter
		// need not.
		if !ok || uses[v] < 2 && !isParam(s.fd, v) {
			return mr13Cand{}, false
		}
		pname := sig.Params().At(i).Name()
		if pname == "" || pname == "_" {
			pname = "_" + strconv.Itoa(i)
		}
		x = mr13Cand{arg: id, v: v, ck: ck}
		for _, k := range slices.Sorted(maps.Keys(s.cv.Body)) {
			if strings.HasPrefix(k, ck+"/actual_in:") && strings.Split(s.cv.Body[k], "|")[2] == pname {
				x.vk = k
			}
		}
	}
	return x, x.arg != nil && x.vk != ""
}

// isParam reports whether v is a parameter, receiver or result of fd.
func isParam(fd *ast.FuncDecl, v *types.Var) bool {
	for _, fl := range []*ast.FieldList{fd.Recv, fd.Type.Params, fd.Type.Results} {
		if fl == nil {
			continue
		}
		for _, f := range fl.List {
			for _, n := range f.Names {
				if n.Pos() == v.Pos() {
					return true
				}
			}
		}
	}
	return false
}

// ─── MR14: route a call ───────────────────────────────────────────────────────

// MR14 routes one direct call statement `f(args)` per function, cycling
// through three forms:
//
//	(a) through a single-implementation interface:
//	    mr14iN(mr14tN{}).f(args), where mr14tN.f calls f with the same
//	    parameter names;
//	(b) through an in-place closure: func() { f(args) }(), in a function
//	    with no other literal (a new one would renumber closure_N);
//	(c) deferred: defer f(args), only when the call is the function's last
//	    top-level statement, so it runs where it did (at exit, after every
//	    other statement).
//
// Oracle: one-way reachability. For (a), every summary edge of the
// original call site must have its counterpart (same `of` at both ends) at
// the new call site.
//
// Two flows the original has are not semantic and are tightened away
// (found on the fixtures, ruled false positives of the relation):
//   - (c) anywhere but last: a deferred call runs at exit, so what it
//     writes can no longer reach the statements after it
//     (`defer readFull(r, arr[:]); return arr[0]` returns before
//     readFull runs). Restricted to the last statement.
//   - (b): a discarded result reaches the rest of the caller only by the
//     coarse wiring (its actual_out → the call node, its host, spec §4.7)
//     and then the call node's control dependence, which models that the
//     call may panic, an exception edge (out of scope). Inside the
//     closure that path ends at the closure. The original's graph leaves
//     out the out-edges of the routed call's result actual_outs (Site.Cut).
var MR14 = Transform{Name: "MR14", edit: func(c *tctx, f *SrcFile) (*fileEdit, error) {
	if c.pre == nil {
		return nil, fmt.Errorf("MR14 needs the original's analysis")
	}
	if usesPrefix(f, "mr14") {
		return nil, errSkip("name clash")
	}
	info := f.Pkg.TypesInfo
	fe := &fileEdit{}
	var edits []edit
	var tail []piece
	for _, fd := range funcDecls(f) {
		if fd.Body == nil {
			continue
		}
		id := funcIdentity(f, fd)
		cv := c.pre.Calls[id]
		if cv == nil || !cv.L3 {
			fe.skip("MR14: callable: no L3 result")
			continue
		}
		type cand struct {
			call    *ast.CallExpr
			callee  *types.Func
			ck      string
			ifaceOK bool
			last    bool
		}
		var cands []cand
		last := map[ast.Stmt]bool{}
		if n := len(fd.Body.List); n > 0 {
			last[fd.Body.List[n-1]] = true
		}
		stmtLists(fd.Body, func(list []ast.Stmt) {
			for _, st := range list {
				es, ok := st.(*ast.ExprStmt)
				if !ok {
					continue
				}
				call, ok := es.X.(*ast.CallExpr)
				if !ok {
					continue
				}
				var callee *types.Func
				switch fn := ast.Unparen(call.Fun).(type) {
				case *ast.Ident:
					callee, _ = info.Uses[fn].(*types.Func)
				case *ast.SelectorExpr:
					callee, _ = info.Uses[fn.Sel].(*types.Func)
				}
				if callee == nil {
					continue
				}
				ck := f.key(call.Pos())
				if _, ok := cv.Body[ck+"/actual_in:0"]; !ok {
					continue // no resolved callee with vertices
				}
				cands = append(cands, cand{call, callee, ck, ifaceForm(f, call, callee), last[st]})
			}
		})
		if len(cands) == 0 {
			fe.skip("MR14: callable: no direct call statement")
			continue
		}
		x := cands[c.choose(len(cands))]
		// A new literal would renumber the function's later closures
		// (closure_N follows source order), so (b) needs a function
		// without literals.
		noLit := !contains(fd.Body, func(*ast.FuncLit) bool { return true })
		forms := []int{}
		if x.ifaceOK {
			forms = append(forms, 0)
		}
		if noLit {
			forms = append(forms, 1)
		}
		if x.last {
			forms = append(forms, 2)
		}
		if len(forms) == 0 {
			fe.skip("MR14: site: no applicable form")
			continue
		}
		form := forms[c.n%len(forms)]
		a, z := f.off(x.call.Pos()), f.off(x.call.End())
		site := Site{Callable: id}
		switch form {
		case 0:
			n := c.fresh("")
			iface, impl := "mr14i"+n, "mr14t"+n
			sig := x.callee.Type().(*types.Signature)
			var params, names []string
			for i := range sig.Params().Len() {
				p := sig.Params().At(i)
				t, _ := spell(f, p.Type())
				params = append(params, p.Name()+" "+t)
				names = append(names, p.Name())
			}
			var results []string
			for i := range sig.Results().Len() {
				t, _ := spell(f, sig.Results().At(i).Type())
				results = append(results, t)
			}
			msig := "(" + strings.Join(params, ", ") + ")"
			if len(results) > 0 {
				msig += " (" + strings.Join(results, ", ") + ")"
			}
			body := x.callee.Name() + "(" + strings.Join(names, ", ") + ")"
			if len(results) > 0 {
				body = "return " + body
			}
			m := x.callee.Name()
			tail = append(tail, lit("\n\n// "+iface+" routes "+m+" through an interface (MR14).\ntype "+iface+" interface{ "+m+msig+" }\n\ntype "+impl+" struct{}\n\nfunc ("+impl+") "+m+msig+" { "+body+" }\n"))
			// Only the function part is replaced: the arguments keep their
			// text and positions.
			edits = append(edits, edit{a: a, z: f.off(x.call.Lparen), anchor: true, out: []piece{lit(iface + "(" + impl + "{})." + m)}})
			site.Desc = desc(&scope{f: f, fd: fd}, x.call, "call "+m+" through "+iface)
			for _, e := range edgesOf(cv, "sum") {
				if strings.HasPrefix(e.src, x.ck+"/") && strings.HasPrefix(e.dst, x.ck+"/") {
					site.Summary = append(site.Summary, SumCheck{Pre: e.src + " -> " + e.dst, Src: Pre(e.src), Dst: Pre(e.dst)})
				}
			}
		case 1:
			edits = append(edits, insert(a, lit("func() { ")), insert(z, lit(" }()")))
			site.Desc = desc(&scope{f: f, fd: fd}, x.call, "call through a closure")
			for k, b := range cv.Body {
				if strings.HasPrefix(k, x.ck+"/actual_out") && strings.HasPrefix(strings.Split(b, "|")[2], "<return>") {
					site.Cut = append(site.Cut, Node(id, k))
				}
			}
		case 2:
			edits = append(edits, insert(a, lit("defer ")))
			site.Desc = desc(&scope{f: f, fd: fd}, x.call, "defer the call")
		}
		c.n++
		fe.sites = append(fe.sites, site)
		fe.touched = append(fe.touched, id)
	}
	if len(edits) == 0 {
		return fe, errSkip("MR14: no eligible site")
	}
	if len(tail) > 0 {
		edits = append(edits, insert(len(f.Src), tail...))
	}
	ps, err := applyEdits(f, edits)
	fe.pieces = ps
	return fe, err
}}

// ifaceForm reports whether call can be routed through an interface: f is a
// non-generic, non-variadic function of the file's package, called by its
// bare name, with named, distinct parameters whose types (and results') the
// file can spell.
func ifaceForm(f *SrcFile, call *ast.CallExpr, callee *types.Func) bool {
	if _, ok := ast.Unparen(call.Fun).(*ast.Ident); !ok || callee.Pkg() != f.Pkg.Types {
		return false
	}
	sig := callee.Type().(*types.Signature)
	if sig.Recv() != nil || sig.Variadic() || sig.TypeParams() != nil || call.Ellipsis.IsValid() {
		return false
	}
	seen := map[string]bool{}
	for i := range sig.Params().Len() {
		p := sig.Params().At(i)
		if p.Name() == "" || p.Name() == "_" || seen[p.Name()] || p.Name() == callee.Name() {
			return false
		}
		seen[p.Name()] = true
		if _, ok := spell(f, p.Type()); !ok {
			return false
		}
	}
	for i := range sig.Results().Len() {
		if _, ok := spell(f, sig.Results().At(i).Type()); !ok {
			return false
		}
	}
	return sig.Params().Len() > 0
}

// ─── MR15: inline the MR6 helpers back ────────────────────────────────────────

// Inline6 undoes MR6 on its output: each `[var …; ]outs = mr6h_…(args)`
// statement becomes the helper's statements again, and the helpers are
// deleted. MR15 is MR6 then Inline6, which must give back the original
// source byte for byte, and the original analysis exactly.
var Inline6 = Transform{Name: "Inline6", edit: func(c *tctx, f *SrcFile) (*fileEdit, error) {
	helpers := map[string]*ast.FuncDecl{}
	for _, fd := range funcDecls(f) {
		if strings.HasPrefix(fd.Name.Name, Prefix6) {
			helpers[fd.Name.Name] = fd
		}
	}
	if len(helpers) == 0 {
		return nil, errSkip("no MR6 helper")
	}
	fe := &fileEdit{}
	var edits []edit
	for _, fd := range funcDecls(f) {
		if helpers[fd.Name.Name] != nil || fd.Body == nil {
			continue
		}
		stmtLists(fd.Body, func(list []ast.Stmt) {
			for i, st := range list {
				name := helperCalled(st)
				h := helpers[name]
				if h == nil {
					continue
				}
				// MR6 writes one `var` per fresh local: the call's results
				// beyond its arguments.
				start := i - helperFresh(st)
				if start < 0 || slices.ContainsFunc(list[start:i], func(d ast.Stmt) bool { return !isVarDecl(d) }) {
					return
				}
				body := h.Body.List
				if len(body) > 0 {
					if _, ok := body[len(body)-1].(*ast.ReturnStmt); ok {
						body = body[:len(body)-1]
					}
				}
				if len(body) == 0 {
					continue
				}
				edits = append(edits, edit{a: f.off(list[start].Pos()), z: f.off(st.End()),
					out: []piece{cp(f, f.off(body[0].Pos()), f.off(body[len(body)-1].End()))}})
				fe.sites = append(fe.sites, Site{Desc: f.Rel + ": inline " + name})
			}
		})
	}
	for _, h := range helpers {
		a := f.off(h.Doc.Pos()) - 2
		z := f.off(h.End()) + 1
		if a < 0 || string(f.Src[a:a+2]) != "\n\n" || z > len(f.Src) || f.Src[z-1] != '\n' {
			return nil, fmt.Errorf("%s: helper %s is not as MR6 writes it", f.Rel, h.Name.Name)
		}
		edits = append(edits, edit{a: a, z: z})
	}
	ps, err := applyEdits(f, edits)
	fe.pieces = ps
	return fe, err
}}

// helperFresh is the number of fresh locals an MR6 call statement assigns.
func helperFresh(st ast.Stmt) int {
	as, ok := st.(*ast.AssignStmt)
	if !ok {
		return 0
	}
	return len(as.Lhs) - len(as.Rhs[0].(*ast.CallExpr).Args)
}

func isVarDecl(st ast.Stmt) bool {
	d, ok := st.(*ast.DeclStmt)
	if !ok {
		return false
	}
	g, ok := d.Decl.(*ast.GenDecl)
	return ok && g.Tok == token.VAR
}

// helperCalled is the MR6 helper st calls, or "".
func helperCalled(st ast.Stmt) string {
	var call ast.Expr
	switch st := st.(type) {
	case *ast.AssignStmt:
		if len(st.Rhs) == 1 {
			call = st.Rhs[0]
		}
	case *ast.ExprStmt:
		call = st.X
	}
	ce, ok := call.(*ast.CallExpr)
	if !ok {
		return ""
	}
	id, ok := ce.Fun.(*ast.Ident)
	if !ok || !strings.HasPrefix(id.Name, Prefix6) {
		return ""
	}
	return id.Name
}
