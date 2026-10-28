package sdg

import (
	"fmt"
	"go/ast"
	"go/types"
	"slices"

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
// call whose receiver may carry such a value (origins), every
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

// addCHA adds the CHA complement's targets (spec §4.8) to every interface
// call of a body-joined project function (funcs) whose receiver may carry
// a library-originated dynamic type: the origins fixpoint over the whole
// program (cha_origin.go). A target is the method each implementer's method
// set selects: a unit, an interface method (a struct embedding an
// interface), a bodiless declaration, or a library method's external (a
// promoted library method). It never adds a call_graph edge (only VTA's
// callees are edges). Functions are visited in a sorted order and targets
// are kept sorted (d.add), so the result is deterministic.
func (d *dispatch) addCHA(c *cha, funcs map[*ssa.Function]bool) {
	o := newOrigins(c, funcs)
	o.solve()
	for _, fn := range o.order {
		if chaHook != nil {
			chaHook(fn)
		}
		var lo map[ssa.Value]bool
		forCalls(fn, func(call ssa.CallInstruction) {
			cc := call.Common()
			if !cc.IsInvoke() || cc.Method == nil {
				return
			}
			if _, isParam := types.Unalias(cc.Value.Type()).(*types.TypeParam); isParam {
				return
			}
			unit, key, ok := c.j.callSite(call)
			if !ok {
				return
			}
			if lo == nil {
				lo = o.eval(fn) // o.cur is nil: no reader is registered
			}
			if !lo[cc.Value] {
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

// runCHA is addCHA under recover (N1): it reads go/types and SSA in ways
// VTA does not, so an unforeseen shape must not abort the analyzer. On a
// panic every CHA target is dropped — the dispatch is restored to what it
// held before — and the error is returned (degraded no_cha).
func (d *dispatch) runCHA(j *join, in Input, funcs map[*ssa.Function]bool) (err error) {
	saved := make(map[siteKey][]Target, len(d.sites))
	for k, ts := range d.sites {
		saved[k] = slices.Clone(ts) // d.add inserts in place: the backing array is not ours
	}
	newExt := make(map[string]bool, len(d.newExt))
	for k := range d.newExt {
		newExt[k] = true
	}
	defer func() {
		if r := recover(); r != nil {
			d.sites = saved
			for k := range d.newExt {
				if !newExt[k] {
					delete(d.newExt, k)
				}
			}
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	d.addCHA(newCHA(j, in), funcs)
	return nil
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
