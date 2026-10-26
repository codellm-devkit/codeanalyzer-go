package dataflow

import (
	"go/ast"
	"go/types"
	"strings"
)

// Path is a k-limited access path base(.field|[*])* (spec §4.5).
//
//   - Base is the path's identity: the *types.Var of a local, parameter,
//     receiver, named result, capture or package-level variable. Shadowed
//     variables are distinct bases even when spelled alike. nil for a
//     pseudo-base such as "<defer>:12:8", and for an unresolved identifier
//     (a name go/types recorded no object for, in a package with type
//     errors); either's identity is its spelling.
//   - Spell is the emitted spelling: the source name for a local
//     ("p.f.g.*", "ch[*]"), "pkgpath::Name" for a package-level variable
//     ("example.com/lib::V.F"), or the pseudo-base itself.
//
// Steps after the base are ".name" (a field), "[*]" (any index / map key /
// channel element) and the wildcard ".*" (anything below: a dereference, a
// k-truncated tail, or the contents a callee may write). A ".*" is always the
// last step. Path is comparable, so it can be a map key.
type Path struct {
	Base  types.Object
	Spell string
}

// Covers reports whether p covers q: the same base, and p's step list a
// prefix of q's, except that a final wildcard step of p (".*" or "[*]")
// matches any non-empty remaining suffix of q. Steps compare as whole
// segments, so "p.f" does not cover "p.fg"; a wildcard stands for at least
// one step, so "x.f.*" does not cover "x.f"; a non-final "[*]" is a plain
// segment ("m[*].f" does not cover "m[*].g").
//
// Task 10 kills with it (a strong def of p kills every reaching def d with
// p.Covers(d)) and matches uses with it in both directions (a use u links
// from d when d.Covers(u) || u.Covers(d)).
func (p Path) Covers(q Path) bool {
	if p.Base != q.Base {
		return false
	}
	ph, ps := splitSpell(p.Spell)
	qh, qs := splitSpell(q.Spell)
	if ph != qh || len(ps) > len(qs) {
		return false
	}
	for i, s := range ps {
		if i == len(ps)-1 && isWildcard(s) {
			return true // len(qs) > i: a non-empty suffix remains
		}
		if s != qs[i] {
			return false
		}
	}
	return true
}

func isWildcard(step string) bool { return step == ".*" || step == "[*]" }

// splitSpell splits a spelling into its base spelling and its steps. A
// package-level base "pkgpath::Name" may contain dots before the "::"; a
// pseudo-base ("<…>") has no steps.
func splitSpell(s string) (string, []string) {
	if strings.HasPrefix(s, "<") {
		return s, nil
	}
	start := 0
	if i := strings.Index(s, "::"); i >= 0 {
		start = i + 2
	}
	end := strings.IndexAny(s[start:], ".[")
	if end < 0 {
		return s, nil
	}
	head, rest := s[:start+end], s[start+end:]
	var steps []string
	for rest != "" {
		n := 1 + strings.IndexAny(rest[1:], ".[")
		if rest[0] == '[' {
			n = strings.IndexByte(rest, ']') + 1
		} else if n == 0 {
			n = len(rest)
		}
		steps, rest = append(steps, rest[:n]), rest[n:]
	}
	return head, steps
}

// pathB is a path under construction: a base and its untruncated steps.
type pathB struct {
	base  types.Object
	head  string
	steps []string
}

// with returns b extended by one step; nothing is appended after a ".*".
func (b pathB) with(step string) pathB {
	if n := len(b.steps); n > 0 && b.steps[n-1] == ".*" {
		return b
	}
	steps := make([]string, len(b.steps), len(b.steps)+1)
	copy(steps, b.steps)
	b.steps = append(steps, step)
	return b
}

// weak reports whether a write to b may not overwrite all of it: an index
// step (one element of many) or a wildcard (a dereference, a truncated tail).
func (b pathB) weak(k int) bool {
	if len(b.steps) > max(k, 1) {
		return true
	}
	for _, s := range b.steps {
		if isWildcard(s) {
			return true
		}
	}
	return false
}

// limit spells b with at most k steps after the base: a longer path keeps its
// first k-1 steps and ends in the ".*" wildcard as its k-th (k=3:
// p.f.g.h.i → p.f.g.*; k=1: p.f.g → p.*). k < 1 is treated as 1.
func (b pathB) limit(k int) Path {
	k = max(k, 1)
	steps := b.steps
	if len(steps) > k {
		steps = append(steps[:k-1:k-1], ".*")
	}
	return Path{Base: b.base, Spell: b.head + strings.Join(steps, "")}
}

// PathOf returns the k-limited access path e names, and false when e is not a
// path (a literal, a call, a constant, a function, a method value, the blank
// identifier, an expression rooted in a call). info must be non-nil.
//
//   - an identifier naming a variable (not a struct field) is a base;
//   - a field selector x.f appends ".f", spelling out implicit embedded
//     fields (e.f promoted from an embedded T is "e.T.f"); a selector through
//     a pointer reads as Go spells it: p.f and (*p).f are both "p.f". When
//     go/types recorded no selection (x has an invalid type, in a package with
//     type errors), x.f is still x's path plus ".f";
//   - an identifier go/types recorded nothing for (an undeclared name, in a
//     package with type errors) is an unresolved base: nil Base, its name as
//     spelling;
//   - a qualified identifier pkg.V is that package-level variable's base;
//   - x[i] (slice, array, map, string, array pointer) appends "[*]"; a
//     generic instantiation is not a path;
//   - *x appends the ".*" wildcard;
//   - parentheses are transparent.
func PathOf(e ast.Expr, info *types.Info, k int) (Path, bool) {
	b, ok := pathOf(e, info)
	if !ok {
		return Path{}, false
	}
	return b.limit(k), true
}

func pathOf(e ast.Expr, info *types.Info) (pathB, bool) {
	if info == nil {
		return pathB{}, false
	}
	switch e := ast.Unparen(e).(type) {
	case *ast.Ident:
		return baseOf(e, info)

	case *ast.SelectorExpr:
		if b, ok, qualified := qualifiedBase(e, info); qualified {
			return b, ok
		}
		// No selection means type errors left X without a usable type: read
		// .Sel as a field step anyway, so a write to it is not lost.
		sel := info.Selections[e]
		if sel != nil && sel.Kind() != types.FieldVal {
			return pathB{}, false
		}
		x := ast.Unparen(e.X)
		if st, ok := x.(*ast.StarExpr); ok {
			x = st.X // (*p).f is p.f
		}
		b, ok := pathOf(x, info)
		if !ok {
			return pathB{}, false
		}
		if sel != nil {
			for _, name := range embeddedPath(sel) {
				b = b.with("." + name)
			}
		}
		return b.with("." + e.Sel.Name), true

	case *ast.IndexExpr:
		if isType(e.Index, info) {
			return pathB{}, false
		}
		x := ast.Unparen(e.X)
		if st, ok := x.(*ast.StarExpr); ok {
			x = st.X // (*a)[i] is a[i]
		}
		b, ok := pathOf(x, info)
		if !ok {
			return pathB{}, false
		}
		return b.with("[*]"), true

	case *ast.StarExpr:
		b, ok := pathOf(e.X, info)
		if !ok {
			return pathB{}, false
		}
		return b.with(".*"), true
	}
	return pathB{}, false
}

// baseOf is the base path of an identifier naming a variable, or the
// unresolved base (nil, spelled by name) of an identifier go/types recorded
// neither a definition nor a use of.
func baseOf(id *ast.Ident, info *types.Info) (pathB, bool) {
	if id.Name == "_" {
		return pathB{}, false
	}
	if unresolved(id, info) {
		return pathB{head: id.Name}, true
	}
	v, ok := info.ObjectOf(id).(*types.Var)
	if !ok || v.IsField() {
		return pathB{}, false
	}
	return pathB{base: v, head: baseSpell(v)}, true
}

// qualifiedBase is the base of qualified identifier pkg.Name (qualified
// reports whether e is one). A member of a package that failed to import is
// unresolved: nil base, spelled "pkgpath::Name" like a package-level variable.
func qualifiedBase(e *ast.SelectorExpr, info *types.Info) (b pathB, ok, qualified bool) {
	x, isIdent := e.X.(*ast.Ident)
	if !isIdent {
		return pathB{}, false, false
	}
	pn, isPkg := info.Uses[x].(*types.PkgName)
	if !isPkg {
		return pathB{}, false, false
	}
	if unresolved(e.Sel, info) {
		return pathB{head: pn.Imported().Path() + "::" + e.Sel.Name}, true, true
	}
	b, ok = baseOf(e.Sel, info)
	return b, ok, true
}

// unresolved reports whether go/types recorded nothing for id: not in Defs
// (a type switch's symbolic variable is there, with a nil object) and not in
// Uses. Only a package with type errors has such an identifier.
func unresolved(id *ast.Ident, info *types.Info) bool {
	if _, ok := info.Defs[id]; ok {
		return false
	}
	_, ok := info.Uses[id]
	return !ok
}

// baseSpell spells a base variable: "pkgpath::Name" at package level, else
// its name.
func baseSpell(v types.Object) string {
	if isPkgVar(v) {
		return v.Pkg().Path() + "::" + v.Name()
	}
	return v.Name()
}

// BaseSpell is baseSpell for package sdg: the L4 global formals and
// footprints spell a package-level variable as its L3 ddg base.
func BaseSpell(v types.Object) string { return baseSpell(v) }

// IsPkgVar is isPkgVar for package sdg (the L4 global footprints).
func IsPkgVar(obj types.Object) bool { return isPkgVar(obj) }

// isPkgVar reports whether obj is a package-level variable. It checks
// membership of the package scope by identity, which holds for packages
// type-checked from source and imported from export data alike.
func isPkgVar(obj types.Object) bool {
	v, ok := obj.(*types.Var)
	return ok && !v.IsField() && v.Pkg() != nil && v.Pkg().Scope().Lookup(v.Name()) == v
}

// embeddedPath returns the names of the implicit embedded fields a field
// selection walks before its final field.
func embeddedPath(sel *types.Selection) []string {
	idx := sel.Index()
	var out []string
	t := sel.Recv()
	for _, i := range idx[:len(idx)-1] {
		if p, ok := t.Underlying().(*types.Pointer); ok {
			t = p.Elem()
		}
		st, ok := t.Underlying().(*types.Struct)
		if !ok {
			return out
		}
		f := st.Field(i)
		out = append(out, f.Name())
		t = f.Type()
	}
	return out
}
