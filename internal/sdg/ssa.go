package sdg

import (
	"fmt"
	"go/types"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"

	"github.com/codellm-devkit/codeanalyzer-go/internal/utils"
)

// buildHook, when set, runs just before each package's Build (a test seam
// for the recovered-panic path).
var buildHook func(*ssa.Package)

// buildSSA builds go/ssa over the project packages of in.Pkgs (spec §4.8, D2).
//
// ssautil.Packages gets only the project packages, so SSA bodies exist for
// them alone; every dependency (stdlib, modules, vendored code) is created
// from types, without bodies. The mode is InstantiateGenerics (VTA needs it)
// | BuildSerially. Each package is then built on its own, sorted by path,
// under recover:
//
//   - A package that is ill-typed (pkg.IllTyped, which also covers a broken
//     dependency) or that ssautil left nil, or whose Build panicked, gets no
//     SSA: it is listed under one partial_ssa entry.
//   - When no package built, no_ssa follows.
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
	if len(ok) > 0 {
		var spkgs []*ssa.Package
		prog, spkgs = ssautil.Packages(ok, ssa.InstantiateGenerics|ssa.BuildSerially)
		for i, sp := range spkgs {
			if sp == nil {
				failed = append(failed, ok[i].PkgPath)
				continue
			}
			if err := buildPackage(sp); err != nil {
				utils.Warn("L4: SSA build of %s failed: %v", ok[i].PkgPath, err)
				failed = append(failed, ok[i].PkgPath)
				continue
			}
			built = append(built, sp)
		}
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

// projectFuncs is the VTA function set (spec §4.8, VTA scope): the SSA
// functions of the built project packages — package-level functions and
// inits, every declared method (generic origins included), the method-set
// wrappers of their non-generic named types — and, transitively, their
// anonymous functions and every function their code refers to that is
// project code (generic instances of project functions, $bound / $thunk
// wrappers). Functions declared outside the project are left out, so VTA's
// cost stays bounded by the project's size.
func projectFuncs(prog *ssa.Program, pkgs []*ssa.Package) map[*ssa.Function]bool {
	project := make(map[*types.Package]bool, len(pkgs))
	for _, p := range pkgs {
		project[p.Pkg] = true
	}
	funcs := map[*ssa.Function]bool{}
	var work []*ssa.Function
	add := func(fn *ssa.Function) {
		if fn != nil && !funcs[fn] && isProjectFunc(fn, project) {
			funcs[fn] = true
			work = append(work, fn)
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
				for _, op := range instr.Operands(buf[:0]) {
					if g, ok := (*op).(*ssa.Function); ok {
						add(g)
					}
				}
			}
		}
	}
	return funcs
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
