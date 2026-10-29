package sdg

import (
	"go/ast"
	"go/token"
	"go/types"

	"github.com/codellm-devkit/codeanalyzer-go/internal/dataflow"
)

// ProvPointsTo is the prov of every may-alias ddg edge (spec §4.10): the L4
// semantic delta over L3's "ssa" edges and the "reaching-defs" port wiring.
const ProvPointsTo = "points-to"

// AliasOracle answers may-alias queries between two access paths of one
// callable (spec §4.10): MayAlias(d, u) reports whether a write to d may
// change memory a read of u observes, d and u having different bases. The
// interface is frozen so that a points-to analysis (an Andersen solve over
// go/ssa) can replace the type-based MVP without a schema change; prov stays
// "points-to".
//
// The DDG solver (dataflow.BuildDDGAlias) consults it only within its
// one-step offset window, so an oracle need not accept pairs outside it.
type AliasOracle interface {
	MayAlias(a, b dataflow.Path) bool
}

// newAliasOracle builds the oracle of unit u; root is the top-level
// declaration enclosing u.Fn (u.Fn itself for a declaration, the outermost
// enclosing function for a closure), which typeOracle scans for taken
// addresses. nil turns the alias pass off. A test seam: TestAlias_AddsOnly
// compares Run with it off and on.
var newAliasOracle func(u dataflow.Unit, root ast.Node) AliasOracle = newTypeOracle

// typeOracle is the type-based may-alias MVP of one callable (spec §4.10,
// rulings of Task 17 and its fix round 1).
//
// A path is matched through its anchors: a prefix of it, at offset 0 (the
// base) or 1 (the base and its first step), that names one object, with the
// steps after the prefix (rem) addressing memory inside that object. An
// anchor is
//
//   - a reference anchor: the prefix has a reference type (pointer, slice,
//     map, chan, func, interface or type parameter); the object is what it
//     points to. A write reaches the object only through a step after the
//     prefix: a bare def `p = …` (or `x.f = …` at offset 1) rebinds the
//     reference and writes nothing another base observes;
//   - a storage anchor: the prefix is an addressable variable whose address
//     may be taken (taken below); the object is its own storage, so a bare
//     def `x = …` overwrites it. A reference-typed variable whose address is
//     taken has both anchors.
//
// At offset 1 the first step is a field (".f") or element ("[*]") whose
// declared type decides the anchor: a reference-typed field or element is a
// reference anchor (it may share its target with another base: `m :=
// s.cache; m[k] = v` reaches `s.cache[k]`); a field or element whose address
// is taken is a storage anchor. A path that ends at the base, or whose first
// step is the ".*" wildcard, stands for every first step, so every field and
// element of the base's type is tried.
//
// MayAlias(d, u) holds iff a writing anchor of d and an anchor of u may be
// the same object and their rems are prefix-compatible
// (dataflow.StepsCompatible). Two anchors may be the same object iff
//
//   - both are references with compatible types (I1, I2): any two interface
//     (or type-parameter) types; an interface and a type that implements it
//     (a type parameter: that satisfies its constraint); else, on the
//     underlying types, pointers with identical elements, slices with
//     identical elements, a pointer to an array [N]E and a slice []E, maps
//     with identical keys and elements, chans with identical elements
//     whatever their directions, identical signatures;
//   - a reference and a storage of type T (I3): a pointer to T, an
//     interface *T implements, or (T an array [N]E) a slice []E;
//   - never two storages: distinct variables never share storage.
//
// Taken addresses are scanned over root, closures included (an &x on a
// captured x may sit in the encloser): &x, &x.f…, &x[i]… and x[:], x[i:j]
// on an array value make x and (x, its first step) taken; so does a call or
// method value of a pointer-receiver method on an addressable value x or
// x.f… (the callee may keep the pointer). A package-level variable, and
// each of its fields and elements, is always taken: &G may sit in any
// function.
//
// A nil base (a "<defer>:" pseudo path, an unresolved identifier) never
// aliases. Globals are bases like any other; so are a closure's captures.
//
// Known gaps, all false negatives: offsets deeper than one step (&x.f.g
// against a *G; q := x.f.g against x.f.g.h); two paths of one base whose
// prefixes alias (x.f and x.g pointing to one object: the solver only asks
// about different bases); and an interface whose dynamic value is a value
// struct holding a reference (i.* written through the interface against the
// struct's reference field).
type typeOracle struct {
	taken    map[types.Object]bool
	takenSub map[subKey]bool
	anchors  map[dataflow.Path][]anchor
	memo     map[[2]dataflow.Path]bool
}

type subKey struct {
	base types.Object
	step string
}

type anchor struct {
	ref   bool       // a reference anchor; else a storage anchor
	t     types.Type // the reference's type, or the storage's
	rem   []string   // the steps after the prefix (nil: every one)
	write bool       // a def of the path writes inside the object
}

func newTypeOracle(u dataflow.Unit, root ast.Node) AliasOracle {
	o := &typeOracle{
		taken:    map[types.Object]bool{},
		takenSub: map[subKey]bool{},
		anchors:  map[dataflow.Path][]anchor{},
		memo:     map[[2]dataflow.Path]bool{},
	}
	if root != nil && u.Info != nil {
		o.scan(root, u.Info)
	}
	return o
}

// scan records the addresses root takes (typeOracle's doc).
func (o *typeOracle) scan(root ast.Node, info *types.Info) {
	ast.Inspect(root, func(n ast.Node) bool {
		switch e := n.(type) {
		case *ast.UnaryExpr:
			if e.Op == token.AND {
				o.take(e.X, info)
			}
		case *ast.SliceExpr:
			if isArray(info.TypeOf(e.X)) {
				o.take(e.X, info)
			}
		case *ast.SelectorExpr:
			if ptrMethodOnValue(e, info) {
				o.take(e.X, info)
			}
		}
		return true
	})
}

// take records x (an addressed operand) and its base's first step.
func (o *typeOracle) take(x ast.Expr, info *types.Info) {
	p, ok := dataflow.PathOf(x, info, 2)
	if !ok || p.Base == nil {
		return
	}
	o.taken[p.Base] = true
	if s := dataflow.PathSteps(p.Spell); len(s) > 0 && s[0] != ".*" {
		o.takenSub[subKey{p.Base, s[0]}] = true
	}
}

// ptrMethodOnValue reports whether sel is a pointer-receiver method (call or
// value) selected on a non-pointer, non-interface operand: Go takes its
// address implicitly.
func ptrMethodOnValue(sel *ast.SelectorExpr, info *types.Info) bool {
	s := info.Selections[sel]
	if s == nil || s.Kind() != types.MethodVal || s.Indirect() {
		return false
	}
	sig, ok := s.Obj().Type().(*types.Signature)
	if !ok || sig.Recv() == nil {
		return false
	}
	if _, ptr := sig.Recv().Type().Underlying().(*types.Pointer); !ptr {
		return false
	}
	t := info.TypeOf(sel.X)
	if t == nil || types.IsInterface(t) {
		return false
	}
	_, ptr := t.Underlying().(*types.Pointer)
	return !ptr
}

func isArray(t types.Type) bool {
	if t == nil {
		return false
	}
	_, ok := t.Underlying().(*types.Array)
	return ok
}

func (o *typeOracle) MayAlias(d, u dataflow.Path) bool {
	if d.Base == nil || u.Base == nil {
		return false
	}
	if d.Base == u.Base {
		return true
	}
	k := [2]dataflow.Path{d, u}
	if v, ok := o.memo[k]; ok {
		return v
	}
	v := o.mayAlias(d, u)
	o.memo[k] = v
	return v
}

func (o *typeOracle) mayAlias(d, u dataflow.Path) bool {
	ua := o.anchorsOf(u)
	for _, a := range o.anchorsOf(d) {
		if !a.write {
			continue
		}
		for _, b := range ua {
			if sameObject(a, b) && dataflow.StepsCompatible(a.rem, b.rem) {
				return true
			}
		}
	}
	return false
}

// anchorsOf returns p's anchors (typeOracle's doc), memoized.
func (o *typeOracle) anchorsOf(p dataflow.Path) []anchor {
	if as, ok := o.anchors[p]; ok {
		return as
	}
	var as []anchor
	bt := p.Base.Type()
	steps := dataflow.PathSteps(p.Spell)
	global := dataflow.IsPkgVar(p.Base)
	baseRef := refType(bt)
	// A reference-typed base whose address is taken has both anchors: what
	// it points to, and its own storage (`pp := &p; *pp = b` rewrites p, so
	// p.n then reads b.n: fix round 2, N2).
	if baseRef {
		as = append(as, anchor{ref: true, t: bt, rem: steps, write: len(steps) > 0})
	}
	if global || o.taken[p.Base] {
		as = append(as, anchor{t: bt, rem: steps, write: true})
	}
	var firsts []string
	var rem []string
	if len(steps) > 0 && steps[0] != ".*" {
		firsts, rem = steps[:1], steps[1:]
	} else {
		firsts = childSteps(bt) // every first step; rem nil
	}
	for _, s := range firsts {
		ft := childType(bt, s)
		if ft == nil {
			continue
		}
		if refType(ft) {
			// A write through the field's reference needs a step past it: one
			// more listed step, or a wildcard standing for it.
			w := len(steps) >= 2 || len(steps) == 1 && steps[0] == ".*"
			as = append(as, anchor{ref: true, t: ft, rem: rem, write: w})
		}
		if global || o.takenSub[subKey{p.Base, s}] {
			// The field's own storage: written by any def reaching it — not by
			// a bare def of a pointer base, which only rebinds the pointer.
			w := len(steps) > 0 || !baseRef
			as = append(as, anchor{t: ft, rem: rem, write: w})
		}
	}
	o.anchors[p] = as
	return as
}

// childSteps returns every first step a path on a base of type t may take:
// its struct fields (through a pointer) by name, and "[*]" for a container.
func childSteps(t types.Type) []string {
	t = deref(t)
	var out []string
	switch u := t.Underlying().(type) {
	case *types.Struct:
		for i := 0; i < u.NumFields(); i++ {
			out = append(out, "."+u.Field(i).Name())
		}
	case *types.Slice, *types.Array, *types.Map, *types.Chan:
		out = append(out, "[*]")
	}
	return out
}

// childType is the declared type of step s below a base of type t (a field
// through a pointer, an element of a slice, array, array pointer, map or
// chan), nil when there is none.
func childType(t types.Type, s string) types.Type {
	t = deref(t)
	switch u := t.Underlying().(type) {
	case *types.Struct:
		for i := 0; i < u.NumFields(); i++ {
			if "."+u.Field(i).Name() == s {
				return u.Field(i).Type()
			}
		}
	case *types.Slice:
		if s == "[*]" {
			return u.Elem()
		}
	case *types.Array:
		if s == "[*]" {
			return u.Elem()
		}
	case *types.Map:
		if s == "[*]" {
			return u.Elem()
		}
	case *types.Chan:
		if s == "[*]" {
			return u.Elem()
		}
	}
	return nil
}

func deref(t types.Type) types.Type {
	if p, ok := t.Underlying().(*types.Pointer); ok {
		return p.Elem()
	}
	return t
}

// refType reports whether t is a reference kind: pointer, slice, map, chan,
// func, interface or type parameter.
func refType(t types.Type) bool {
	switch t.Underlying().(type) {
	case *types.Pointer, *types.Slice, *types.Map, *types.Chan, *types.Signature, *types.Interface:
		return true
	}
	return false
}

func sameObject(a, b anchor) bool {
	switch {
	case a.ref && b.ref:
		return refsMayAlias(a.t, b.t)
	case a.ref:
		return refToStorage(a.t, b.t)
	case b.ref:
		return refToStorage(b.t, a.t)
	}
	return false
}

// refsMayAlias reports whether two references of types a and b may point to
// one object (I1, I2: typeOracle's doc).
func refsMayAlias(a, b types.Type) bool {
	ia, ib := types.IsInterface(a), types.IsInterface(b)
	switch {
	case ia && ib:
		return true
	case ia:
		return implementsIface(b, a)
	case ib:
		return implementsIface(a, b)
	}
	if arrayPtrSlice(a, b) || arrayPtrSlice(b, a) {
		return true
	}
	switch ua := a.Underlying().(type) {
	case *types.Pointer:
		ub, ok := b.Underlying().(*types.Pointer)
		return ok && types.Identical(ua.Elem(), ub.Elem())
	case *types.Slice:
		ub, ok := b.Underlying().(*types.Slice)
		return ok && types.Identical(ua.Elem(), ub.Elem())
	case *types.Map:
		ub, ok := b.Underlying().(*types.Map)
		return ok && types.Identical(ua.Key(), ub.Key()) && types.Identical(ua.Elem(), ub.Elem())
	case *types.Chan:
		ub, ok := b.Underlying().(*types.Chan)
		return ok && types.Identical(ua.Elem(), ub.Elem())
	case *types.Signature:
		return types.Identical(ua, b.Underlying())
	}
	return false
}

// arrayPtrSlice reports whether a is a pointer to an array [N]E and b a
// slice []E: pa[:] shares the array (fix round 2, O1).
func arrayPtrSlice(a, b types.Type) bool {
	pa, ok := a.Underlying().(*types.Pointer)
	if !ok {
		return false
	}
	arr, ok := pa.Elem().Underlying().(*types.Array)
	if !ok {
		return false
	}
	sl, ok := b.Underlying().(*types.Slice)
	return ok && types.Identical(arr.Elem(), sl.Elem())
}

// refToStorage reports whether a reference of type r may point to the
// storage of a value of type t (I3: typeOracle's doc).
func refToStorage(r, t types.Type) bool {
	if types.IsInterface(r) {
		return implementsIface(types.NewPointer(t), r)
	}
	switch ur := r.Underlying().(type) {
	case *types.Pointer:
		return types.Identical(ur.Elem(), t)
	case *types.Slice:
		at, ok := t.Underlying().(*types.Array)
		return ok && types.Identical(ur.Elem(), at.Elem())
	}
	return false
}

// implementsIface reports whether a value of type t may be held by a value
// of interface type iface: t implements it, or (iface a type parameter)
// satisfies its constraint.
func implementsIface(t, iface types.Type) bool {
	if tp, ok := iface.(*types.TypeParam); ok {
		c, ok := tp.Constraint().Underlying().(*types.Interface)
		return ok && types.Satisfies(t, c)
	}
	it, ok := iface.Underlying().(*types.Interface)
	return ok && types.Implements(t, it)
}

// rootOf returns the top-level declaration enclosing unit u's function: u.Fn
// for a declaration, else the outermost unit function of u's file whose
// extent contains u.Fn (a package-level literal is its own root).
func (p *ports) rootOf(u dataflow.Unit) ast.Node {
	if _, ok := u.Fn.(*ast.FuncDecl); ok || u.Fn == nil {
		return u.Fn
	}
	if p.fnsByPath == nil {
		p.fnsByPath = map[string][]ast.Node{}
		for _, v := range p.in.Units {
			if v.Fn != nil {
				p.fnsByPath[v.Key.Path] = append(p.fnsByPath[v.Key.Path], v.Fn)
			}
		}
	}
	root := u.Fn
	for _, fn := range p.fnsByPath[u.Key.Path] {
		if fn.Pos() <= root.Pos() && root.End() <= fn.End() && fn.Pos() < root.Pos() {
			root = fn
		}
	}
	return root
}
