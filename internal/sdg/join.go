package sdg

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/ssa"

	"github.com/codellm-devkit/codeanalyzer-go/internal/dataflow"
	"github.com/codellm-devkit/codeanalyzer-go/internal/semantic_analysis"
	"github.com/codellm-devkit/codeanalyzer-go/internal/syntactic_analysis"
	"github.com/codellm-devkit/codeanalyzer-go/internal/utils"
)

// SSA Synthetic tags the join tells apart (x/tools v0.52 go/ssa).
const (
	synthPackageInit = "package initializer"
	synthYield       = "range-over-func yield"
	synthInstWrapper = "instantiation wrapper"
)

// joinKind is how an SSA function joined its callable.
type joinKind int

const (
	joinNone    joinKind = iota
	joinBody             // its code is the callable's body: by syntax, origin, or a folded yield
	joinWrapper          // a synthetic wrapper (or a bodiless stub) of the callable: by Object()
)

// join maps go/ssa back to the L3 units (spec §4.8, SSA → L3 join). It keys
// only on AST pointer identity and raw Lparen positions, never on line:col or
// SSA names, so //line directives, SSA's anonymous-function numbering and
// build order cannot move a mapping. Its maps are read-only after newJoin, so
// it is safe for concurrent use, and it never builds SSA code (it may be
// used after a no_vta).
type join struct {
	fset       *token.FileSet
	projectDir string
	byFn       map[ast.Node]dataflow.L3Key             // Unit.Fn → unit
	bySig      map[dataflow.L3Key]bool                 // (declaring file, signature) of every unit and interface method
	calls      map[dataflow.L3Key]map[token.Pos]string // unit → call Lparen → L1/L3 call key
	projPaths  map[string]bool                         // import paths of the project packages
	units      map[dataflow.L3Key]dataflow.Unit        // by key
	results    map[dataflow.L3Key]dataflow.Result      // in.L3
	pkgs       map[string]*packages.Package            // in.Pkgs
}

// newJoin indexes in's units. A unit's call keys are its L3 result's
// CallKeys; a unit with no result (l3_skipped), or one built without side
// tables, falls back to its L1 call keys (Unit.Calls), which the emitter
// writes either way.
func newJoin(in Input) *join {
	j := &join{
		fset:       in.Fset,
		projectDir: in.ProjectDir,
		byFn:       make(map[ast.Node]dataflow.L3Key, len(in.Units)),
		bySig:      make(map[dataflow.L3Key]bool, len(in.Units)),
		calls:      make(map[dataflow.L3Key]map[token.Pos]string, len(in.Units)),
		projPaths:  map[string]bool{},
		units:      make(map[dataflow.L3Key]dataflow.Unit, len(in.Units)),
		results:    in.L3,
		pkgs:       in.Pkgs,
	}
	for _, u := range in.Units {
		j.byFn[u.Fn] = u.Key
		j.bySig[u.Key] = true
		j.units[u.Key] = u
		if r, ok := in.L3[u.Key]; ok && r.CallKeys != nil {
			j.calls[u.Key] = r.CallKeys
			continue
		}
		m := make(map[token.Pos]string, len(u.Calls))
		for _, c := range u.Calls {
			m[c.Lparen] = c.Key
		}
		j.calls[u.Key] = m
	}
	// Interface methods have no body and so no unit, but a $bound wrapper of
	// one (a method value of an interface) still names it: index them by
	// (declaring file, signature) too, the file being the interface's.
	for path, f := range in.SymbolTable {
		for _, t := range f.Types {
			for _, c := range t.Methods {
				if c.Path == "" {
					j.bySig[dataflow.L3Key{Path: path, Signature: c.Signature}] = true
				}
			}
		}
	}
	for _, p := range projectPackages(in) {
		j.projPaths[p.PkgPath] = true
	}
	return j
}

// fnUnit returns the L3 callable fn joins:
//
//  1. fn.Syntax() is a unit's Fn (a declared function or method, init#N,
//     a function literal, a generic instance or instantiation wrapper —
//     both keep their origin's syntax; a wrapper joins as a wrapper).
//  2. Else, for a top-level function, fn.Origin()'s syntax: an instance
//     with no syntax of its own (go/ssa v0.52 always copies it, so this is
//     a guard). Closures never call Origin: for a closure of an instance it
//     may build a package.
//  3. Else, for a function with no syntax — a synthetic wrapper ($bound,
//     $thunk, promoted or pointer wrapper) or a bodiless stub of a project
//     function seen through another test variant of its package — the
//     function fn.Object() names, by (declaring file, SignatureOf), the
//     symbol table's own canonicalizer. An interface method's key (the
//     interface's file) counts here too.
//  4. A range-over-func yield folds into fn.Parent().
//
// The package initializer, closures of package-level variable initializers
// and "from type information" functions declared outside the project map to
// nothing.
func (j *join) fnUnit(fn *ssa.Function) (dataflow.L3Key, bool) {
	k, kind := j.resolve(fn)
	return k, kind != joinNone
}

func (j *join) resolve(fn *ssa.Function) (dataflow.L3Key, joinKind) {
	for fn != nil && fn.Synthetic == synthYield {
		fn = fn.Parent()
	}
	if fn == nil {
		return dataflow.L3Key{}, joinNone
	}
	if syn := fn.Syntax(); syn != nil {
		k, ok := j.byFn[syn]
		switch {
		case !ok:
			return dataflow.L3Key{}, joinNone
		case strings.HasPrefix(fn.Synthetic, synthInstWrapper):
			// It keeps its origin's syntax, but its body is a synthetic
			// call of the origin.
			return k, joinWrapper
		}
		return k, joinBody
	}
	if fn.Parent() == nil {
		if o := fn.Origin(); o != nil && o != fn {
			if k, ok := j.byFn[o.Syntax()]; ok {
				return k, joinBody
			}
		}
	}
	if fn.Synthetic == synthPackageInit {
		return dataflow.L3Key{}, joinNone
	}
	if obj, ok := fn.Object().(*types.Func); ok {
		if k, ok := j.declKey(obj.Origin()); ok && j.bySig[k] {
			return k, joinWrapper
		}
	}
	return dataflow.L3Key{}, joinNone
}

// declKey is obj's (declaring file, signature) when it is declared in a
// project file.
func (j *join) declKey(obj *types.Func) (dataflow.L3Key, bool) {
	tf := j.fset.File(obj.Pos())
	if tf == nil || !utils.IsWithin(j.projectDir, tf.Name()) {
		return dataflow.L3Key{}, false
	}
	rel := utils.RelativePath(j.projectDir, tf.Name())
	if utils.IsVendored(rel) {
		return dataflow.L3Key{}, false
	}
	return dataflow.L3Key{Path: rel, Signature: syntactic_analysis.SignatureOf(obj)}, true
}

// callSite returns the unit and L1/L3 call key of instr: its function must
// join by body (a wrapper's calls are synthetic) and its CallCommon.Pos() —
// the CallExpr's Lparen — must be one of the unit's call keys. A call with
// no position (one go/ssa synthesizes: the defer stack, a nil check, an
// init call, a range-over-func iterator call) maps to nothing.
func (j *join) callSite(instr ssa.CallInstruction) (unit dataflow.L3Key, callKey string, ok bool) {
	pos := instr.Common().Pos()
	if !pos.IsValid() {
		return dataflow.L3Key{}, "", false
	}
	k, kind := j.resolve(instr.Parent())
	if kind != joinBody {
		return dataflow.L3Key{}, "", false
	}
	key, ok := j.calls[k][pos]
	if !ok {
		return dataflow.L3Key{}, "", false
	}
	return k, key, true
}

// externalOf returns the full callee signature (the app.Externals key) of a
// function fn.Object() names outside the project: a library function or
// method, a wrapper of one, or its "from type information" stub. The
// signature is L2's (semantic_analysis.CalleeSignatureOf), so L2 and L4
// external ids agree.
func (j *join) externalOf(fn *ssa.Function) (sig string, ok bool) {
	obj, ok := fn.Object().(*types.Func)
	if !ok || obj.Pkg() == nil || !j.isLibrary(obj) {
		return "", false
	}
	return semantic_analysis.CalleeSignatureOf(obj), true
}

// isLibrary mirrors the L2 rule: declared in a file outside the project or
// vendored; without a file, in a package that is not a project package.
func (j *join) isLibrary(obj *types.Func) bool {
	if tf := j.fset.File(obj.Pos()); tf != nil {
		if !utils.IsWithin(j.projectDir, tf.Name()) {
			return true
		}
		return utils.IsVendored(utils.RelativePath(j.projectDir, tf.Name()))
	}
	return !j.projPaths[obj.Pkg().Path()]
}

// joinCoverage counts how the project functions and their call instructions
// joined. Only call instructions with a source position count: a call
// go/ssa synthesizes has none and no call node.
type joinCoverage struct {
	Funcs, Unmapped, Wrappers, Inits int // Inits: package initializers (no callable by design)
	Calls, UnmappedCalls, Synthetic  int // Calls: positioned calls in body-joined functions
	unmappedPkgs                     map[string]bool
}

// coverage joins every function of funcs (the project functions) and its
// calls. A package initializer has no callable (L1 records none for
// package-level variable initializers) and its calls are not counted; a
// closure in such an initializer is unmapped.
func (j *join) coverage(funcs map[*ssa.Function]bool) joinCoverage {
	c := joinCoverage{unmappedPkgs: map[string]bool{}}
	for fn := range funcs {
		if fn.Synthetic == synthPackageInit {
			c.Inits++
			continue
		}
		c.Funcs++
		_, kind := j.resolve(fn)
		switch kind {
		case joinNone:
			c.Unmapped++
			c.unmappedPkgs[homePath(fn)] = true
			utils.Debug("L4: unmapped SSA function %s [%s]", fn, fn.Synthetic)
		case joinWrapper:
			c.Wrappers++
		}
		forCalls(fn, func(call ssa.CallInstruction) {
			switch {
			case !call.Common().Pos().IsValid():
				c.Synthetic++
			case kind == joinWrapper:
				c.Synthetic++
			default:
				c.Calls++
				if _, _, ok := j.callSite(call); !ok {
					c.UnmappedCalls++
					c.unmappedPkgs[homePath(fn)] = true
					utils.Debug("L4: unmapped SSA call %s in %s at %s", call, fn, j.fset.PositionFor(call.Common().Pos(), false))
				}
			}
		})
	}
	return c
}

// degraded is the unmapped_ssa entry for c, or nil. A listed package with
// cgo files gets a note: cgo files are not in the symbol table yet (#29).
func (j *join) degraded(c joinCoverage) *Degraded {
	if c.Unmapped == 0 && c.UnmappedCalls == 0 {
		return nil
	}
	pkgs := sortedKeys(c.unmappedPkgs)
	detail := fmt.Sprintf("%d SSA function(s) and %d call instruction(s) map to no callable or call node", c.Unmapped, c.UnmappedCalls)
	for _, p := range pkgs {
		if hasCgo(j.fset, j.pkgs[p]) {
			detail += "; cgo files are not in the symbol table (#29)"
			break
		}
	}
	return &Degraded{Code: CodeUnmappedSSA, Detail: detail, Packages: pkgs}
}

// hasCgo reports whether p compiles cgo-generated files: go/packages then
// parses the generated files (in the build cache) instead of the ones in
// GoFiles. The load does not request CompiledGoFiles, so the test is a
// Syntax file that GoFiles does not list.
func hasCgo(fset *token.FileSet, p *packages.Package) bool {
	if p == nil {
		return false
	}
	files := make(map[string]bool, len(p.GoFiles))
	for _, f := range p.GoFiles {
		files[f] = true
	}
	for _, f := range p.Syntax {
		if tf := fset.File(f.Pos()); tf != nil && !files[tf.Name()] {
			return true
		}
	}
	return false
}

// forCalls calls f on every call instruction of fn's body.
func forCalls(fn *ssa.Function, f func(ssa.CallInstruction)) {
	for _, b := range fn.Blocks {
		for _, instr := range b.Instrs {
			if call, ok := instr.(ssa.CallInstruction); ok {
				f(call)
			}
		}
	}
}

// homePath is the import path of the package fn's code comes from: its own
// package, its generic origin's, its enclosing function's, or (for a
// wrapper) the wrapped object's.
func homePath(fn *ssa.Function) string {
	for f := fn; f != nil; f = f.Parent() {
		if f.Pkg != nil {
			return f.Pkg.Pkg.Path()
		}
		if f.Parent() == nil {
			if o := f.Origin(); o != nil && o.Pkg != nil {
				return o.Pkg.Pkg.Path()
			}
			if obj := f.Object(); obj != nil && obj.Pkg() != nil {
				return obj.Pkg().Path()
			}
		}
	}
	return ""
}

// l3Skipped is the l3_skipped entry (spec §4.15, A16): the units with no L3
// result (a recovered panic, or a body call with no L1 key), or nil.
func l3Skipped(in Input) *Degraded {
	var keys []dataflow.L3Key
	for _, u := range in.Units {
		if _, ok := in.L3[u.Key]; !ok {
			keys = append(keys, u.Key)
		}
	}
	if len(keys) == 0 {
		return nil
	}
	sortKeys(keys)
	return &Degraded{
		Code:      CodeL3Skipped,
		Detail:    fmt.Sprintf("%d callable(s) have no L3 result (a recovered panic or a call with no L1 key)", len(keys)),
		Callables: keys,
	}
}

// rangeFunc is the range_func entry (spec §4.15, D8): the units whose own
// body (not a nested literal: that is its own unit) ranges over a function,
// and their packages, or nil. It reads the AST only, so it holds whether or
// not SSA was built.
func rangeFunc(in Input) *Degraded {
	pkgOf := map[string]string{}
	for _, p := range projectPackages(in) {
		for _, f := range p.GoFiles {
			pkgOf[f] = p.PkgPath
		}
	}
	var keys []dataflow.L3Key
	pkgs := map[string]bool{}
	for _, u := range in.Units {
		if !rangesOverFunc(u) {
			continue
		}
		keys = append(keys, u.Key)
		if tf := in.Fset.File(u.Fn.Pos()); tf != nil {
			if p, ok := pkgOf[tf.Name()]; ok {
				pkgs[p] = true
			}
		}
	}
	if len(keys) == 0 {
		return nil
	}
	sortKeys(keys)
	return &Degraded{
		Code:      CodeRangeFunc,
		Detail:    "range-over-func is not modelled at L4 (D8): what the iterator passes to yield does not reach the loop variables",
		Packages:  sortedKeys(pkgs),
		Callables: keys,
	}
}

func rangesOverFunc(u dataflow.Unit) bool {
	var body *ast.BlockStmt
	switch fn := u.Fn.(type) {
	case *ast.FuncDecl:
		body = fn.Body
	case *ast.FuncLit:
		body = fn.Body
	}
	if body == nil || u.Info == nil {
		return false
	}
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.RangeStmt:
			if dataflow.RangesOverFunc(n.X, u.Info) {
				found = true
			}
		}
		return !found
	})
	return found
}

func sortKeys(keys []dataflow.L3Key) {
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Path != keys[j].Path {
			return keys[i].Path < keys[j].Path
		}
		return keys[i].Signature < keys[j].Signature
	})
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
