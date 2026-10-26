package sdg

import (
	"fmt"
	"go/types"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"
	"golang.org/x/tools/go/types/typeutil"

	"github.com/codellm-devkit/codeanalyzer-go/internal/utils"
)

// buildHook, when set, runs just before each package's Build (a test seam
// for the recovered-panic path).
var buildHook func(*ssa.Package)

// callHook, when set, runs on the static callee of every call instruction SSA
// emits, inside the build (a test seam for panics mid-build: after a builder
// has created shared generic instances, or while a wrapper is built on demand).
var callHook func(*types.Func)

// installCallHook wires callHook into prog through the builder's no-return
// predicate, which SSA consults on every static call it emits. Production
// runs never set callHook, so prog keeps its nil predicate.
func installCallHook(prog *ssa.Program) {
	if hook := callHook; hook != nil {
		prog.SetNoReturn(func(f *types.Func) bool { hook(f); return false })
	}
}

// buildSSA builds go/ssa over the project packages of in.Pkgs (spec §4.8, D2).
//
// ssautil.Packages gets only the project packages, so SSA bodies exist for
// them alone; every dependency (stdlib, modules, vendored code) is created
// from types, without bodies. The mode is InstantiateGenerics (VTA needs it)
// | BuildSerially. Each package is then built on its own, sorted by path,
// under recover:
//
//   - A package that is ill-typed (pkg.IllTyped, which also covers a broken
//     dependency) or that ssautil left nil gets no SSA.
//   - A package whose Build panics gets no SSA either, and the whole program
//     is discarded and rebuilt without it (it then enters from types, as a
//     dependency). A builder that panics never marks the generic instances
//     and wrappers it created as done, so any later build or method lookup
//     sharing one would block forever — not a panic, and not recoverable.
//     The rebuild repeats until a pass builds every remaining package, at
//     most once per package.
//   - Every package that got no SSA is listed under one partial_ssa entry;
//     when no package built, no_ssa follows.
//
// It returns the built packages in path order.
func buildSSA(in Input) (*ssa.Program, []*ssa.Package, []Degraded) {
	roots := projectPackages(in)
	var failed []string
	var ok []*packages.Package
	for _, p := range roots {
		if p.IllTyped {
			failed = append(failed, p.PkgPath)
			continue
		}
		ok = append(ok, p)
	}

	var prog *ssa.Program
	var built []*ssa.Package
	for len(ok) > 0 {
		var spkgs []*ssa.Package
		prog, spkgs = ssautil.Packages(ok, ssa.InstantiateGenerics|ssa.BuildSerially)
		installCallHook(prog)
		built = built[:0]
		var good []*packages.Package
		panicked := -1
		for i, sp := range spkgs {
			if sp == nil {
				failed = append(failed, ok[i].PkgPath)
				continue
			}
			if err := buildPackage(sp); err != nil {
				utils.Debug("L4: SSA build of %s failed: %v; rebuilding without it", ok[i].PkgPath, err)
				failed = append(failed, ok[i].PkgPath)
				panicked = i
				break
			}
			good = append(good, ok[i])
			built = append(built, sp)
		}
		if panicked < 0 {
			break
		}
		// Drop this (possibly wedged) program; rebuild from the packages not
		// yet known bad: the ones that built, and the ones not reached.
		ok = append(good, ok[panicked+1:]...)
		prog, built = nil, nil
	}

	var deg []Degraded
	if len(failed) > 0 {
		sort.Strings(failed)
		deg = append(deg, Degraded{
			Code:     CodePartialSSA,
			Detail:   fmt.Sprintf("%d package(s) have no SSA (type errors or a recovered build panic)", len(failed)),
			Packages: failed,
		})
	}
	if len(built) == 0 && len(roots) > 0 {
		deg = append(deg, Degraded{Code: CodeNoSSA, Detail: "no project package could be built in SSA form"})
	}
	return prog, built, deg
}

// buildPackage builds one SSA package, turning a panic into an error.
func buildPackage(p *ssa.Package) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	if buildHook != nil {
		buildHook(p)
	}
	p.Build()
	return nil
}

// projectPackages returns the packages of in.Pkgs that hold project code (a
// Go file under ProjectDir, not vendored), sorted by path. Generated test
// mains live in the build cache and so are never project packages.
func projectPackages(in Input) []*packages.Package {
	var out []*packages.Package
	for _, p := range in.Pkgs {
		if isProject(in.ProjectDir, p) {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PkgPath < out[j].PkgPath })
	return out
}

func isProject(dir string, p *packages.Package) bool {
	for _, f := range p.GoFiles {
		if utils.IsWithin(dir, f) && !utils.IsVendored(utils.RelativePath(dir, f)) {
			return true
		}
	}
	return false
}

// projectFuncs is the VTA function set (spec §4.8, VTA scope). funcs holds
// the SSA functions of the built project packages — package-level functions
// and inits, every declared method (generic origins included), the method-set
// wrappers of their non-generic named types — and, transitively, their
// anonymous functions and every function their code refers to that is project
// code (generic instances of project functions, $bound / $thunk wrappers).
// Its code is what VTA propagates types through, so VTA's cost stays bounded
// by the project's size; the SSA join is measured on it.
//
// targetOnly holds the functions declared outside the project that a dynamic
// call in project code may reach, which VTA can return only when they are in
// its set (it intersects the propagated types with a lazy CHA over the set):
//
//   - a library function used as a value in project code (any operand but
//     the static callee of a call: VTA resolves static calls without the set);
//   - the methods (prog.MethodValue) of each type that a MakeInterface in
//     project code converts to an interface — the dynamic types an interface
//     call there may dispatch to — that are not project code: a library
//     type's methods, or a project type's wrappers of promoted library
//     methods. A project method found this way joins funcs.
//
// A library function has no SSA body (its package is created from types), so
// targetOnly adds no propagation work; a library promotion wrapper's body is
// a single synthetic call.
func projectFuncs(prog *ssa.Program, pkgs []*ssa.Package) (funcs, targetOnly map[*ssa.Function]bool) {
	project := make(map[*types.Package]bool, len(pkgs))
	for _, p := range pkgs {
		project[p.Pkg] = true
	}
	funcs = map[*ssa.Function]bool{}
	targetOnly = map[*ssa.Function]bool{}
	var work []*ssa.Function
	add := func(fn *ssa.Function) {
		if fn != nil && !funcs[fn] && isProjectFunc(fn, project) {
			funcs[fn] = true
			work = append(work, fn)
		}
	}
	addTarget := func(fn *ssa.Function) {
		switch {
		case fn == nil || funcs[fn]:
		case isProjectFunc(fn, project):
			add(fn)
		default:
			targetOnly[fn] = true
		}
	}
	var converted typeutil.Map // types already expanded by a MakeInterface
	addMethods := func(t types.Type) {
		if converted.At(t) != nil {
			return
		}
		converted.Set(t, true)
		if _, isParam := types.Unalias(t).(*types.TypeParam); isParam || types.IsInterface(t) {
			return
		}
		mset := prog.MethodSets.MethodSet(t)
		for i := 0; i < mset.Len(); i++ {
			addTarget(prog.MethodValue(mset.At(i))) // nil for a generic method or type
		}
	}
	for _, p := range pkgs {
		names := make([]string, 0, len(p.Members))
		for name := range p.Members {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			switch m := p.Members[name].(type) {
			case *ssa.Function:
				add(m)
			case *ssa.Type:
				named, ok := types.Unalias(m.Type()).(*types.Named)
				if !ok {
					continue
				}
				for i := 0; i < named.NumMethods(); i++ {
					add(prog.FuncValue(named.Method(i)))
				}
				if named.TypeParams() == nil && !types.IsInterface(named) {
					for _, t := range []types.Type{named, types.NewPointer(named)} {
						mset := prog.MethodSets.MethodSet(t)
						for i := 0; i < mset.Len(); i++ {
							add(prog.MethodValue(mset.At(i)))
						}
					}
				}
			}
		}
	}
	var buf [10]*ssa.Value
	for len(work) > 0 {
		fn := work[len(work)-1]
		work = work[:len(work)-1]
		for _, anon := range fn.AnonFuncs {
			add(anon)
		}
		for _, b := range fn.Blocks {
			for _, instr := range b.Instrs {
				var callee *ssa.Value
				if call, ok := instr.(ssa.CallInstruction); ok && !call.Common().IsInvoke() {
					callee = &call.Common().Value
				}
				for _, op := range instr.Operands(buf[:0]) {
					g, ok := (*op).(*ssa.Function)
					switch {
					case !ok:
					case op == callee:
						add(g) // a static callee: VTA needs it only as project code
					default:
						addTarget(g)
					}
				}
				if mi, ok := instr.(*ssa.MakeInterface); ok {
					addMethods(mi.X.Type())
				}
			}
		}
	}
	return funcs, targetOnly
}

// isProjectFunc reports whether fn's code is project code: it belongs to a
// built project package, is an instance (or a closure of one) of a project
// generic, or is a synthetic wrapper ($bound, $thunk, promotion, pointer
// indirection) of a project method.
func isProjectFunc(fn *ssa.Function, project map[*types.Package]bool) bool {
	for f := fn; f != nil; f = f.Parent() {
		if f.Pkg != nil {
			return project[f.Pkg.Pkg]
		}
		if o := f.Origin(); o != nil && o.Pkg != nil {
			return project[o.Pkg.Pkg]
		}
		if f.Parent() == nil {
			if strings.HasPrefix(f.Synthetic, "from type information") {
				return false
			}
			if obj := f.Object(); obj != nil {
				return project[obj.Pkg()]
			}
		}
	}
	return false
}
