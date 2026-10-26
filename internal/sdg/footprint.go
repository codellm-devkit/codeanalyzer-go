package sdg

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"slices"
	"strings"

	"golang.org/x/tools/go/packages"

	"github.com/codellm-devkit/codeanalyzer-go/internal/dataflow"
	"github.com/codellm-devkit/codeanalyzer-go/internal/schema"
	"github.com/codellm-devkit/codeanalyzer-go/internal/utils"
)

// Footprint is what a call target touches beyond its formals' values (spec
// §4.6, §4.9): the package-level variables it reads and writes, itself or
// through any callee, spelled "pkgpath::Name" (the L3 ddg base spelling), and
// the params and receiver it may write through (by-ref outs), by base name,
// or by the D6 `of` (`_<i>`, `_recv`) for an unnamed one. Each list is sorted
// and deduplicated. Lists may be shared between footprints: callers must not
// modify them.
type Footprint struct {
	GlobalReads, GlobalWrites []string
	Mutated                   []string
}

// globals is a callable's global read and write sets (sorted spellings).
type globals struct{ reads, writes []string }

// footprints computes the footprint of every unit and of every dispatch
// target (spec §4.9). It reads the L3 side tables, the dispatch and go/types
// only, never the SSA program, so it is safe after a no_vta.
//
//   - A unit with an L3 result: its direct globals are the package-level
//     variable bases of its DefUse (uses are reads; strong and weak defs are
//     writes; @entry, which defines every global the body touches, is not a
//     write) and every global it takes the address of (&v, or a pointer
//     method value v.M): a write through that address is not a def of v at
//     L3. Globals the filter drops (globalFilter, A5) are removed. Its
//     footprint is propagated bottom-up over the call graph (callGraph), so
//     it holds every global any callee touches.
//   - Mutated: a receiver or param p whose type may hold a reference
//     (dataflow.MayHoldRef, A10) and that some node other than @entry
//     defines a path through (p.f, p.*, p[*]: an assignment, a call-rule weak
//     def where p is a by-ref argument or receiver, a capture write). It is
//     not propagated: the L3 call rule already weak-defines p.* at a call
//     passing p, so the caller's own DefUse records it.
//   - A stand-in, an interface method, a bodiless declaration and a unit
//     with no L3 result (or no type info): no globals, and Mutated = every
//     by-ref formal (byRefFormals).
func footprints(in Input, d *dispatch) map[Target]Footprint {
	nodes, callees := callGraph(in, d)
	gf := newGlobalFilter(in)
	formals := newFormalIndex(in, d)

	direct := map[Target]globals{}
	mutated := map[Target][]string{}
	for _, u := range in.Units {
		if !analysed(in, u) {
			gf.mentions(u) // no DefUse: every global it names may be written
			continue
		}
		t := Target{Callable: u.Key}
		r := in.L3[u.Key]
		direct[t] = gf.direct(u, r)
		mutated[t] = mutatedFormals(u, r)
	}
	gf.initializers()
	for t, g := range direct {
		direct[t] = globals{reads: gf.keep(g.reads), writes: gf.keep(g.writes)}
	}

	prop := propagate(nodes, callees, direct)
	out := make(map[Target]Footprint, len(nodes))
	for _, t := range nodes {
		if _, ok := direct[t]; ok {
			g := prop[t]
			out[t] = Footprint{GlobalReads: g.reads, GlobalWrites: g.writes, Mutated: mutated[t]}
			continue
		}
		out[t] = Footprint{Mutated: byRefFormals(formals.of(t))}
	}
	return out
}

// analysed reports whether unit u has the L3 facts footprints read: an L3
// result with its DefUse side table, and type info.
func analysed(in Input, u dataflow.Unit) bool {
	r, ok := in.L3[u.Key]
	return ok && r.DefUse != nil && u.Info != nil
}

// callGraph returns the call graph footprints propagate over (spec §4.9):
// every unit and every dispatch target, sorted by compareTarget, and each
// unit's callees, the union of its call sites' dispatch targets (L2's static
// callee ∪ VTA ∪ the interface method, D7), sorted. A call node the CFG
// pruned keeps its targets (an over-approximation). A unit footprints cannot
// analyse (analysed) has no callees: its footprint carries no globals.
func callGraph(in Input, d *dispatch) ([]Target, map[Target][]Target) {
	callees := map[Target][]Target{}
	nodeSet := map[Target]bool{}
	ok := map[dataflow.L3Key]bool{}
	for _, u := range in.Units {
		nodeSet[Target{Callable: u.Key}] = true
		ok[u.Key] = analysed(in, u)
	}
	for site, ts := range d.sites {
		for _, t := range ts {
			nodeSet[t] = true
		}
		if !ok[site.unit] {
			continue
		}
		u := Target{Callable: site.unit}
		callees[u] = append(callees[u], ts...)
	}
	for u, ts := range callees {
		slices.SortFunc(ts, compareTarget)
		callees[u] = slices.Compact(ts)
	}
	nodes := make([]Target, 0, len(nodeSet))
	for t := range nodeSet {
		nodes = append(nodes, t)
	}
	slices.SortFunc(nodes, compareTarget)
	return nodes, callees
}

// propagate closes the direct global sets over callees, bottom-up over the
// SCCs (callees first): a target's set is the union of the direct sets of
// every target it reaches, itself included. That is the least fixpoint of
// "a target's set ⊇ its own ∪ each callee's"; every member of an SCC reaches
// every other, so the fixpoint gives all members one set, the union of the
// members' direct sets and of the sets of the SCCs they call (computed
// earlier), and one pass per SCC reaches it. When the union adds nothing to
// one contribution, that contribution's slice is shared rather than copied.
func propagate(nodes []Target, callees map[Target][]Target, direct map[Target]globals) map[Target]globals {
	comps := sccs(nodes, func(t Target) []Target { return callees[t] })
	compOf := make(map[Target]int, len(nodes))
	out := make(map[Target]globals, len(nodes))
	for ci, comp := range comps {
		for _, m := range comp {
			compOf[m] = ci
		}
		var reads, writes [][]string
		for _, m := range comp {
			if g, ok := direct[m]; ok {
				reads, writes = append(reads, g.reads), append(writes, g.writes)
			}
			for _, c := range callees[m] {
				if compOf[c] != ci {
					g := out[c]
					reads, writes = append(reads, g.reads), append(writes, g.writes)
				}
			}
		}
		g := globals{reads: unionSorted(reads), writes: unionSorted(writes)}
		for _, m := range comp {
			out[m] = g
		}
	}
	return out
}

// unionSorted is the sorted union of the sorted, deduplicated lists. It
// returns the largest list itself when the union is no larger, and nil when
// it is empty.
func unionSorted(lists [][]string) []string {
	var largest []string
	n := 0
	for _, l := range lists {
		if len(l) > len(largest) {
			largest = l
		}
		n += len(l)
	}
	if n == len(largest) {
		if len(largest) == 0 {
			return nil
		}
		return largest
	}
	all := make([]string, 0, n)
	for _, l := range lists {
		all = append(all, l...)
	}
	slices.Sort(all)
	all = slices.Compact(all)
	if len(all) == len(largest) {
		return largest
	}
	return slices.Clip(all)
}

// mutatedFormals is the Mutated set of unit u from its L3 result r (A10):
// the receiver and params that may hold a reference and that a node other
// than @entry defines a path through, by name, sorted.
func mutatedFormals(u dataflow.Unit, r dataflow.Result) []string {
	byRef := map[types.Object]bool{}
	for _, obj := range formalObjs(u.Fn, u.Info) {
		if dataflow.MayHoldRef(obj.Type()) {
			byRef[obj] = true
		}
	}
	if len(byRef) == 0 {
		return nil
	}
	hit := map[string]bool{}
	for key, du := range r.DefUse {
		if key == dataflow.EntryKey {
			continue
		}
		for _, list := range [][]dataflow.Path{du.Defs, du.WeakDefs} {
			for _, p := range list {
				// A path with a step after its base: the base's own spelling is
				// its name (a param is never a package-level variable).
				if byRef[p.Base] && len(p.Spell) > len(p.Base.Name()) {
					hit[p.Base.Name()] = true
				}
			}
		}
	}
	return sortedKeys(hit)
}

// formalObjs returns the receiver and parameter objects of fn (a
// *ast.FuncDecl or *ast.FuncLit), in declaration order; "_" and unnamed ones
// have none.
func formalObjs(fn ast.Node, info *types.Info) []types.Object {
	var lists []*ast.FieldList
	switch fn := fn.(type) {
	case *ast.FuncDecl:
		lists = []*ast.FieldList{fn.Recv, fn.Type.Params}
	case *ast.FuncLit:
		lists = []*ast.FieldList{fn.Type.Params}
	}
	var out []types.Object
	for _, fl := range lists {
		if fl == nil {
			continue
		}
		for _, f := range fl.List {
			for _, name := range f.Names {
				if obj := info.Defs[name]; obj != nil && name.Name != "_" {
					out = append(out, obj)
				}
			}
		}
	}
	return out
}

// globalFilter is the A5 global filter (spec §4.6): a package-level
// variable is kept iff it is declared outside the project (os::Args), or
// some project code writes it — a strong or weak def in any unit (init and
// closures included), &v or a pointer method value, a write in a unit L3
// could not analyse, or a write in a package-level initializer — or its
// initializer contains a call.
type globalFilter struct {
	fset      *token.FileSet
	pkgs      []*packages.Package
	project   map[string]bool // import paths of the project packages
	kept      map[string]bool // project globals that pass, by spelling
	spellings map[types.Object]string
}

func newGlobalFilter(in Input) *globalFilter {
	f := &globalFilter{
		fset:      in.Fset,
		pkgs:      projectPackages(in),
		project:   map[string]bool{},
		kept:      map[string]bool{},
		spellings: map[types.Object]string{},
	}
	for _, p := range f.pkgs {
		f.project[p.PkgPath] = true
	}
	return f
}

// spell returns the spelling of obj when it is a package-level variable.
func (f *globalFilter) spell(obj types.Object) (string, bool) {
	if s, ok := f.spellings[obj]; ok {
		return s, s != ""
	}
	s := ""
	if dataflow.IsPkgVar(obj) {
		s = dataflow.BaseSpell(obj)
	}
	f.spellings[obj] = s
	return s, s != ""
}

// write records a write of obj when it is a project global.
func (f *globalFilter) write(obj types.Object) {
	if s, ok := f.spell(obj); ok && f.project[obj.Pkg().Path()] {
		f.kept[s] = true
	}
}

// keep returns the globals of list (sorted spellings) the filter keeps.
func (f *globalFilter) keep(list []string) []string {
	var out []string
	for _, s := range list {
		pkg, _, _ := strings.Cut(s, "::")
		if !f.project[pkg] || f.kept[s] {
			out = append(out, s)
		}
	}
	return out
}

// direct returns the unfiltered direct globals of unit u (see footprints)
// and records its writes of project globals.
func (f *globalFilter) direct(u dataflow.Unit, r dataflow.Result) globals {
	reads, writes := map[string]bool{}, map[string]bool{}
	read := func(obj types.Object) {
		if s, ok := f.spell(obj); ok {
			reads[s] = true
		}
	}
	write := func(obj types.Object) {
		if s, ok := f.spell(obj); ok {
			writes[s] = true
			f.write(obj)
		}
	}
	for key, du := range r.DefUse {
		if key == dataflow.EntryKey {
			continue
		}
		for _, p := range du.Uses {
			read(p.Base)
		}
		for _, list := range [][]dataflow.Path{du.Defs, du.WeakDefs} {
			for _, p := range list {
				write(p.Base)
			}
		}
	}
	addressTaken(u.Fn, u.Info, write)
	return globals{reads: sortedKeys(reads), writes: sortedKeys(writes)}
}

// addressTaken calls f on the base of every &x and of every pointer method
// value x.M (a call x.M() included) whose receiver x is not a pointer: the
// address may be written through. It walks fn's body, not its function
// literals: they are their own units.
func addressTaken(fn ast.Node, info *types.Info, f func(types.Object)) {
	var body *ast.BlockStmt
	switch fn := fn.(type) {
	case *ast.FuncDecl:
		body = fn.Body
	case *ast.FuncLit:
		body = fn.Body
	}
	if body == nil {
		return
	}
	base := func(e ast.Expr) {
		if p, ok := dataflow.PathOf(e, info, 1); ok && p.Base != nil {
			f(p.Base)
		}
	}
	ast.Inspect(body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.UnaryExpr:
			if n.Op == token.AND {
				base(n.X)
			}
		case *ast.SelectorExpr:
			sel := info.Selections[n]
			if sel == nil || sel.Kind() != types.MethodVal {
				break
			}
			if m, ok := sel.Obj().(*types.Func); ok {
				if recv := m.Signature().Recv(); recv != nil && isPtr(recv.Type()) && !isPtr(info.TypeOf(n.X)) {
					base(n.X)
				}
			}
		}
		return true
	})
}

func isPtr(t types.Type) bool {
	if t == nil {
		return false
	}
	_, ok := t.Underlying().(*types.Pointer)
	return ok
}

// mentions records every project global unit u (nested literals included)
// names as written: with no DefUse, any of them may be.
func (f *globalFilter) mentions(u dataflow.Unit) {
	if u.Info == nil {
		// No type info: nothing to resolve a name against. Keep every
		// global the unit's package declares.
		f.keepPackageOf(u)
		return
	}
	ast.Inspect(u.Fn, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok {
			f.write(u.Info.Uses[id])
		}
		return true
	})
}

// keepPackageOf keeps every package-level variable of the project package
// that declares u.
func (f *globalFilter) keepPackageOf(u dataflow.Unit) {
	tf := f.fset.File(u.Fn.Pos())
	if tf == nil {
		return
	}
	for _, p := range f.pkgs {
		if !slices.Contains(p.GoFiles, tf.Name()) || p.Types == nil {
			continue
		}
		scope := p.Types.Scope()
		for _, name := range scope.Names() {
			f.write(scope.Lookup(name))
		}
	}
}

// initializers applies the filter's rules to the package-level variable
// declarations of every project package: a variable whose initializer
// contains a call (not a conversion or a builtin, and not in a function
// literal's body: that runs only when the literal is called) is kept; so is
// every project global an initializer may write — one named inside a call's
// operands, under &, or in a function literal (L1 has no callable for a
// package-level literal, so no DefUse says what it writes).
func (f *globalFilter) initializers() {
	for _, p := range f.pkgs {
		info := p.TypesInfo
		if info == nil {
			continue
		}
		for _, file := range p.Syntax {
			for _, decl := range file.Decls {
				gd, ok := decl.(*ast.GenDecl)
				if !ok || gd.Tok != token.VAR {
					continue
				}
				for _, spec := range gd.Specs {
					vs := spec.(*ast.ValueSpec)
					for _, v := range vs.Values {
						if hasCall(v, info) {
							for _, name := range vs.Names {
								f.write(info.Defs[name])
							}
						}
						f.initWrites(v, info, false)
					}
				}
			}
		}
	}
}

// hasCall reports whether e calls a function outside a function literal's
// body: a conversion or a builtin call does not count, but its operands are
// searched.
func hasCall(e ast.Expr, info *types.Info) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.CallExpr:
			if tv, ok := info.Types[n.Fun]; !ok || !(tv.IsType() || tv.IsBuiltin()) {
				found = true
			}
		}
		return !found
	})
	return found
}

// initWrites records as written every project global n names in a hot
// position: inside a call (its function value and operands), under &, or in
// a function literal.
func (f *globalFilter) initWrites(n ast.Node, info *types.Info, hot bool) {
	ast.Inspect(n, func(m ast.Node) bool {
		switch m := m.(type) {
		case *ast.Ident:
			if hot {
				f.write(info.Uses[m])
			}
		case *ast.CallExpr:
			if !hot {
				f.initWrites(m, info, true)
				return false
			}
		case *ast.FuncLit:
			f.initWrites(m.Body, info, true)
			return false
		case *ast.UnaryExpr:
			if m.Op == token.AND && !hot {
				f.initWrites(m.X, info, true)
				return false
			}
		}
		return true
	})
}

// formal is one receiver or declared parameter of a call target: its D6
// `of` name and its type (nil when only the type's string is known).
type formal struct {
	name     string
	typ      types.Type
	typStr   string
	recv     bool
	variadic bool
}

// byRefFormals returns the names of the formals that are by-ref (the shared
// predicate dataflow.MayHoldRef on a known type, else refTypeString),
// sorted.
func byRefFormals(fs []formal) []string {
	var out []string
	for _, f := range fs {
		if f.typ != nil && dataflow.MayHoldRef(f.typ) || f.typ == nil && (f.variadic || refTypeString(f.typStr)) {
			out = append(out, f.name)
		}
	}
	slices.Sort(out)
	return out
}

// refTypeString is the by-ref predicate on a type known only by its string
// (spec §4.6, the conservative fallback): false only for a basic type, or an
// array of one; a named type that cannot be resolved may hold a reference.
func refTypeString(s string) bool {
	for strings.HasPrefix(s, "[") {
		i := strings.IndexByte(s, ']')
		if i <= 1 { // "[]T": a slice
			return true
		}
		s = s[i+1:]
	}
	if b, ok := types.Universe.Lookup(s).(*types.TypeName); ok {
		if _, basic := b.Type().(*types.Basic); basic {
			return false
		}
	}
	return true
}

// formalIndex finds the formals of call targets, with their go/types types
// wherever the load has them:
//
//   - a unit: its Fn's signature (types.Info);
//   - an interface method or a bodiless declaration (no unit): the
//     *types.Func a project package's scope declares under that key;
//   - an external: the names from its GoExternalCallee (already D6), the
//     types from the *types.Func its import path's package declares, found
//     through the load's import graph (every external is reached from
//     project code, so its package is loaded, from source or export data).
//
// When no type is found, the symbol table's or the external's type strings
// are used with refTypeString.
type formalIndex struct {
	in        Input
	units     map[dataflow.L3Key]dataflow.Unit
	decls     map[dataflow.L3Key]*types.Func // project funcs and methods by (declaring file, signature)
	strs      map[dataflow.L3Key]schema.GoCallable
	ext       map[string]schema.GoExternalCallee
	pkgByPath map[string]*types.Package
}

func newFormalIndex(in Input, d *dispatch) *formalIndex {
	x := &formalIndex{
		in:        in,
		units:     make(map[dataflow.L3Key]dataflow.Unit, len(in.Units)),
		decls:     map[dataflow.L3Key]*types.Func{},
		strs:      map[dataflow.L3Key]schema.GoCallable{},
		ext:       map[string]schema.GoExternalCallee{},
		pkgByPath: map[string]*types.Package{},
	}
	for _, u := range in.Units {
		x.units[u.Key] = u
	}
	for sig, e := range in.Externals {
		x.ext[sig] = e
	}
	for sig, e := range d.newExt {
		x.ext[sig] = e
	}
	var walk func(path string, c schema.GoCallable)
	walk = func(path string, c schema.GoCallable) {
		x.strs[dataflow.L3Key{Path: path, Signature: c.Signature}] = c
		for _, ic := range c.InnerCallables {
			walk(ic.Path, ic)
		}
	}
	for path, f := range in.SymbolTable {
		for _, c := range f.Functions {
			walk(c.Path, c)
		}
		for _, t := range f.Types {
			for _, c := range t.Methods {
				if c.Path == "" { // an interface method, keyed by its interface's file
					walk(path, c)
					continue
				}
				walk(c.Path, c)
			}
		}
	}
	j := &join{fset: in.Fset, projectDir: in.ProjectDir} // declKey reads only these two
	for _, p := range projectPackages(in) {
		if p.Types == nil {
			continue
		}
		scope := p.Types.Scope()
		for _, name := range scope.Names() {
			switch obj := scope.Lookup(name).(type) {
			case *types.Func:
				x.addDecl(j, obj)
			case *types.TypeName:
				named, ok := obj.Type().(*types.Named)
				if !ok {
					continue
				}
				for i := 0; i < named.NumMethods(); i++ {
					x.addDecl(j, named.Method(i))
				}
				if it, ok := named.Underlying().(*types.Interface); ok {
					for i := 0; i < it.NumExplicitMethods(); i++ {
						x.addDecl(j, it.ExplicitMethod(i))
					}
				}
			}
		}
	}
	var visit func(p *packages.Package)
	visit = func(p *packages.Package) {
		if p == nil || p.Types == nil || x.pkgByPath[p.PkgPath] != nil {
			return
		}
		x.pkgByPath[p.PkgPath] = p.Types
		for _, q := range p.Imports {
			visit(q)
		}
	}
	for _, p := range in.Pkgs {
		visit(p)
	}
	return x
}

func (x *formalIndex) addDecl(j *join, fn *types.Func) {
	if k, ok := j.declKey(fn); ok {
		x.decls[k] = fn
	}
}

// of returns t's receiver (first) and declared params.
func (x *formalIndex) of(t Target) []formal {
	if t.External != "" {
		return x.external(t.External)
	}
	if u, ok := x.units[t.Callable]; ok && u.Info != nil {
		if sig := unitSignature(u); sig != nil {
			return sigFormals(sig)
		}
	}
	if fn, ok := x.decls[t.Callable]; ok {
		return sigFormals(fn.Signature())
	}
	c, ok := x.strs[t.Callable]
	if !ok {
		utils.Debug("L4: no formals for %v", t)
		return nil
	}
	var out []formal
	if c.ReceiverType != "" {
		out = append(out, formal{name: recvName(c.ReceiverName), typStr: c.ReceiverType, recv: true})
	}
	for i, p := range c.Parameters {
		out = append(out, formal{name: paramName(p.Name, i), typStr: p.Type, variadic: p.IsVariadic})
	}
	return out
}

// external returns the formals of the external with full signature sig.
func (x *formalIndex) external(sig string) []formal {
	e, ok := x.ext[sig]
	if !ok {
		utils.Debug("L4: no external %s", sig)
		return nil
	}
	var out []formal
	if e.Receiver != nil {
		out = append(out, formal{name: e.Receiver.Name, typStr: e.Receiver.Type, recv: true})
	}
	for _, p := range e.Params {
		out = append(out, formal{name: p.Name, typStr: p.Type, variadic: p.IsVariadic})
	}
	if fn := x.libraryFunc(e); fn != nil {
		if typed := sigFormals(fn.Signature()); len(typed) == len(out) {
			for i := range out {
				out[i].typ = typed[i].typ
			}
		}
	}
	return out
}

// libraryFunc looks up e's *types.Func: a function of its package's scope,
// or a method (Sig "Type.Method") of a type it declares.
func (x *formalIndex) libraryFunc(e schema.GoExternalCallee) *types.Func {
	pkg := x.pkgByPath[e.ImportPath]
	if pkg == nil {
		return nil
	}
	typ, method, isMethod := strings.Cut(e.Sig, ".")
	if !isMethod {
		fn, _ := pkg.Scope().Lookup(e.Sig).(*types.Func)
		return fn
	}
	tn, ok := pkg.Scope().Lookup(typ).(*types.TypeName)
	if !ok {
		return nil
	}
	obj, _, _ := types.LookupFieldOrMethod(tn.Type(), true, pkg, method)
	fn, _ := obj.(*types.Func)
	return fn
}

// unitSignature is the signature of u's Fn.
func unitSignature(u dataflow.Unit) *types.Signature {
	switch fn := u.Fn.(type) {
	case *ast.FuncDecl:
		if f, ok := u.Info.Defs[fn.Name].(*types.Func); ok {
			return f.Signature()
		}
	case *ast.FuncLit:
		sig, _ := u.Info.TypeOf(fn).(*types.Signature)
		return sig
	}
	return nil
}

// sigFormals returns sig's receiver (an interface method's receiver is its
// interface) and params, named by D6.
func sigFormals(sig *types.Signature) []formal {
	var out []formal
	if r := sig.Recv(); r != nil {
		out = append(out, formal{name: recvName(r.Name()), typ: r.Type(), recv: true})
	}
	for i := 0; i < sig.Params().Len(); i++ {
		p := sig.Params().At(i)
		out = append(out, formal{name: paramName(p.Name(), i), typ: p.Type(), variadic: sig.Variadic() && i == sig.Params().Len()-1})
	}
	return out
}

// recvName and paramName are the D6 `of` of a receiver and of declared
// param i (receiver excluded).
func recvName(name string) string {
	if name == "" || name == "_" {
		return "_recv"
	}
	return name
}

func paramName(name string, i int) string {
	if name == "" || name == "_" {
		return fmt.Sprintf("_%d", i)
	}
	return name
}
