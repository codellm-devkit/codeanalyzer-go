package sdg

import (
	"go/ast"
	"go/token"
	"go/types"

	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/types/typeutil"

	"github.com/codellm-devkit/codeanalyzer-go/internal/semantic_analysis"
)

// cha is the targeted CHA complement of VTA (spec §4.8). VTA propagates
// types through project code only: library packages have no SSA bodies, so
// a value that passes through library storage — put in a context.Context or
// a sync.Map, returned by a library call, read from a library struct's field
// — reaches an interface call with no concrete type VTA can see, and only
// the D7 interface method (empty footprint) would remain. For an interface
// call whose receiver originates in library code (libraryOrigin), every
// project type whose method set implements the interface is a target: the
// method it resolves to, mapped as the join maps a callee. It reads SSA
// instructions and go/types only, never builds code, so it is safe before
// VTA.
type cha struct {
	j       *join
	types   []types.Type // every project named type T and *T that is not an interface or generic
	byIface typeutil.Map // interface type → its implementers among types
	msets   typeutil.MethodSetCache
}

func newCHA(j *join, in Input) *cha {
	c := &cha{j: j}
	seen := map[*types.TypeName]bool{}
	for _, p := range projectPackages(in) {
		if p.TypesInfo == nil {
			continue
		}
		for _, f := range p.Syntax {
			ast.Inspect(f, func(n ast.Node) bool {
				ts, ok := n.(*ast.TypeSpec)
				if !ok {
					return true
				}
				tn, ok := p.TypesInfo.Defs[ts.Name].(*types.TypeName)
				if !ok || tn.IsAlias() || seen[tn] {
					return true
				}
				seen[tn] = true
				named, ok := tn.Type().(*types.Named)
				if !ok || named.TypeParams() != nil || types.IsInterface(named) {
					return true
				}
				c.types = append(c.types, named, types.NewPointer(named))
				return true
			})
		}
	}
	return c
}

// implementers returns the project types of c.types that implement iface
// (T and *T alike: an interface may hold either).
func (c *cha) implementers(iface types.Type) []types.Type {
	if v := c.byIface.At(iface); v != nil {
		return v.([]types.Type)
	}
	it, _ := iface.Underlying().(*types.Interface)
	var out []types.Type
	if it != nil {
		for _, t := range c.types {
			if types.Implements(t, it) {
				out = append(out, t)
			}
		}
	}
	c.byIface.Set(iface, out)
	return out
}

// addCHA adds the CHA complement's targets to every interface call of a
// body-joined project function (funcs) whose receiver originates in library
// code. A target is the method each implementer's method set selects:
// a unit, an interface method (a struct embedding an interface), a
// bodiless declaration, or a library method's external (a promoted library
// method). It never adds a call_graph edge (only VTA's callees are edges).
func (d *dispatch) addCHA(c *cha, funcs map[*ssa.Function]bool) {
	for fn := range funcs {
		forCalls(fn, func(call ssa.CallInstruction) {
			cc := call.Common()
			if !cc.IsInvoke() || cc.Method == nil {
				return
			}
			if _, isParam := types.Unalias(cc.Value.Type()).(*types.TypeParam); isParam {
				return
			}
			unit, key, ok := c.j.callSite(call)
			if !ok || !c.libraryOrigin(cc.Value, map[ssa.Value]bool{}) {
				return
			}
			site := siteKey{unit, key}
			for _, t := range c.implementers(cc.Value.Type()) {
				sel := c.msets.MethodSet(t).Lookup(cc.Method.Pkg(), cc.Method.Name())
				if sel == nil {
					continue
				}
				if tg, ok := d.funcTarget(c.j, sel.Obj().(*types.Func)); ok {
					d.add(site, tg)
				}
			}
		})
	}
}

// funcTarget maps a declared function or method to its target, as
// calleeTarget maps its SSA function: the unit it declares, an interface
// method, a bodiless declaration, or a library function's external. A
// universe method (error.Error, promoted through an embedded error) has no
// package and no target, as for methodTarget.
func (d *dispatch) funcTarget(j *join, obj *types.Func) (Target, bool) {
	if obj.Pkg() == nil {
		return Target{}, false
	}
	if j.isLibrary(obj) {
		sig := semantic_analysis.CalleeSignatureOf(obj)
		d.external(obj, sig)
		return Target{External: sig}, true
	}
	k, ok := declKey(j.fset, j.projectDir, obj.Origin())
	if !ok {
		return Target{}, false
	}
	if _, unit := j.units[k]; unit || j.iface[k] || j.bodiless[k] {
		return Target{Callable: k}, true
	}
	return Target{}, false
}

// libraryOrigin reports whether v, an interface call's receiver, may hold a
// value whose dynamic type VTA cannot see: following Phi edges, Extract,
// ChangeInterface and loads back to
//
//   - a TypeAssert (the asserted value came from anywhere);
//   - the result of a call to a library function or of an interface call of
//     a library interface's method (its callee may be library code);
//   - a load of a library global, or through a library struct's field, or
//     through a pointer that itself originates in library code.
//
// A value project code built (MakeInterface), a parameter, a free variable
// or a load of project storage is VTA's: false. The remaining gap is a
// library-originated value first stored in a project struct field or
// global and then read back (spec §4.8).
func (c *cha) libraryOrigin(v ssa.Value, seen map[ssa.Value]bool) bool {
	if seen[v] {
		return false
	}
	seen[v] = true
	switch v := v.(type) {
	case *ssa.Phi:
		for _, e := range v.Edges {
			if c.libraryOrigin(e, seen) {
				return true
			}
		}
	case *ssa.Extract:
		return c.libraryOrigin(v.Tuple, seen)
	case *ssa.ChangeInterface:
		return c.libraryOrigin(v.X, seen)
	case *ssa.TypeAssert:
		return true
	case *ssa.Call:
		return c.libraryCall(v.Common())
	case *ssa.UnOp:
		if v.Op == token.MUL {
			return c.libraryLoad(v.X, seen)
		}
	case *ssa.Field:
		if c.libraryType(v.X.Type()) {
			return true
		}
		return c.libraryOrigin(v.X, seen)
	}
	return false
}

// libraryCall reports whether cc calls a library function, or a library
// interface's method.
func (c *cha) libraryCall(cc *ssa.CallCommon) bool {
	if cc.IsInvoke() {
		return cc.Method != nil && cc.Method.Pkg() != nil && c.j.isLibrary(cc.Method)
	}
	callee := cc.StaticCallee()
	if callee == nil {
		return false
	}
	obj, ok := callee.Object().(*types.Func)
	return ok && obj.Pkg() != nil && c.j.isLibrary(obj)
}

// libraryLoad reports whether a load through the pointer x reads library
// storage, or a pointer that originates in library code.
func (c *cha) libraryLoad(x ssa.Value, seen map[ssa.Value]bool) bool {
	switch x := x.(type) {
	case *ssa.Global:
		return x.Pkg != nil && !c.j.projPaths[x.Pkg.Pkg.Path()]
	case *ssa.FieldAddr:
		if ptr, ok := x.X.Type().Underlying().(*types.Pointer); ok && c.libraryType(ptr.Elem()) {
			return true
		}
		return c.libraryOrigin(x.X, seen)
	case *ssa.IndexAddr:
		return c.libraryOrigin(x.X, seen)
	}
	return c.libraryOrigin(x, seen)
}

// libraryType reports whether t is a named type declared outside the
// project.
func (c *cha) libraryType(t types.Type) bool {
	named, ok := types.Unalias(t).(*types.Named)
	if !ok {
		return false
	}
	pkg := named.Obj().Pkg()
	return pkg != nil && !c.j.projPaths[pkg.Path()]
}
