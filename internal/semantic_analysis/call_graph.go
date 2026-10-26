// Package semantic_analysis builds the resolver-based call graph (Level 1, Tier 1).
//
// This stage uses the same golang.org/x/tools/go/packages load that built the
// symbol table. For each recorded call site it resolves the callee to a
// *types.Func, derives its signature via SignatureOf(), backfills
// callee_signature in place, and emits an identity-only GoCallEdge.
//
// Precision choice: declared-type dispatch (CHA-style). Pointer-receiver
// methods are followed; interface dispatch records the interface method
// signature and falls back gracefully when the concrete type is unknown.
package semantic_analysis

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"

	"github.com/codellm-devkit/codeanalyzer-go/internal/schema"
	"github.com/codellm-devkit/codeanalyzer-go/internal/syntactic_analysis"
	"github.com/codellm-devkit/codeanalyzer-go/internal/utils"
)

// CallGraphBuilder resolves call sites and builds the call graph.
type CallGraphBuilder struct {
	projectDir string
	fset       *token.FileSet
	pkgs       map[string]*packages.Package
	indexes    map[*packages.Package]callSiteIndex
	externals  map[string]schema.GoExternalCallee
}

// NewCallGraphBuilder creates a builder using the same pkgs/fset loaded for the symbol table.
func NewCallGraphBuilder(projectDir string, fset *token.FileSet, pkgs map[string]*packages.Package) *CallGraphBuilder {
	return &CallGraphBuilder{projectDir: projectDir, fset: fset, pkgs: pkgs, indexes: map[*packages.Package]callSiteIndex{}}
}

// Externals returns the library callees recorded by Build, keyed by the full
// callee signature. Nil before Build or when no library call was seen.
func (cg *CallGraphBuilder) Externals() map[string]schema.GoExternalCallee { return cg.externals }

// Build resolves all call sites in symbolTable, backfills callee_signature, and
// returns the identity-only edge list. Never crashes on unresolved sites — it logs
// and skips the edge while leaving callee_signature nil.
func (cg *CallGraphBuilder) Build(symbolTable map[string]schema.GoFile) []schema.GoCallEdge {
	var edges []schema.GoCallEdge
	seen := map[edgeKey]bool{}
	cg.externals = nil

	// Build the known-sig set once. It separates in-project callees from library
	// callees, which are recorded in cg.externals rather than dropped.
	knownSigs := buildKnownSigs(symbolTable)

	// Visit files, types and callables in sorted key order so the edge list and
	// the first-writer content of cg.externals never depend on map iteration.
	for _, fileKey := range sortedKeys(symbolTable) {
		goFile := symbolTable[fileKey]
		// Find the package for this file.
		pkg := cg.packageForFile(fileKey)
		if pkg == nil || pkg.TypesInfo == nil {
			continue
		}

		// Resolve methods on types.
		for _, typeSig := range sortedKeys(goFile.Types) {
			goType := goFile.Types[typeSig]
			for _, methSig := range sortedKeys(goType.Methods) {
				newCallable, newEdges := cg.resolveCallable(pkg, goType.Methods[methSig], seen, knownSigs)
				goType.Methods[methSig] = newCallable
				edges = append(edges, newEdges...)
			}
			goFile.Types[typeSig] = goType
		}

		// Resolve package-level functions.
		for _, fnSig := range sortedKeys(goFile.Functions) {
			newCallable, newEdges := cg.resolveCallable(pkg, goFile.Functions[fnSig], seen, knownSigs)
			goFile.Functions[fnSig] = newCallable
			edges = append(edges, newEdges...)
		}

		symbolTable[fileKey] = goFile
	}

	return edges
}

// edgeKey dedups call edges per source callable. The declaring file is part of
// the key because a signature is not unique: every file of a package may declare
// its own func init(), and all of them share the signature "<pkg>.init".
type edgeKey struct{ path, src, dst string }

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// buildKnownSigs returns the set of all callable signatures present in the symbol table.
func buildKnownSigs(symbolTable map[string]schema.GoFile) map[string]bool {
	sigs := make(map[string]bool)
	for _, f := range symbolTable {
		for sig := range f.Functions {
			sigs[sig] = true
		}
		for _, t := range f.Types {
			for sig := range t.Methods {
				sigs[sig] = true
			}
		}
	}
	return sigs
}

// resolveCallable backfills callee_signature on each call site and produces edges.
// It then does the same for the callable's closures (InnerCallables) in sorted
// key order, recursively, after the callable's own sites.
//
// An in-project callee (in knownSigs) gets an edge. A library callee (a package
// outside the project) is recorded in cg.externals, its site is tagged with the
// callee's import path, and it gets an edge too (target = the full callee
// signature). A callee in a project package that is not in the symbol table
// (e.g. an interface method) is backfilled but gets no edge.
func (cg *CallGraphBuilder) resolveCallable(
	pkg *packages.Package,
	callable schema.GoCallable,
	seen map[edgeKey]bool,
	knownSigs map[string]bool,
) (schema.GoCallable, []schema.GoCallEdge) {
	var edges []schema.GoCallEdge

	for i := range callable.CallSites {
		site := &callable.CallSites[i]
		if site.CalleeSignature != nil {
			continue // already resolved
		}

		fn := cg.resolveCallSite(pkg, callable.Path, site)
		if fn == nil {
			utils.Debug("unresolved call site: %s in %s", site.MethodName, callable.Signature)
			continue
		}
		calleeSig := CalleeSignatureOf(fn)

		// Backfill the site whether the callee is in-project or a library.
		site.CalleeSignature = &calleeSig

		if !knownSigs[calleeSig] {
			if !cg.isLibrary(fn) {
				utils.Debug("in-project callee without a symbol-table entry (no edge): %s", calleeSig)
				continue
			}
			site.CalleeImportPath = fn.Pkg().Path()
			cg.recordExternal(fn, calleeSig)
		}

		key := edgeKey{callable.Path, callable.Signature, calleeSig}
		if seen[key] {
			continue
		}
		seen[key] = true

		edges = append(edges, schema.GoCallEdge{
			Source:     callable.Signature,
			Target:     calleeSig,
			Type:       "CALL_DEP",
			Weight:     1,
			Provenance: []string{"go/types"},
			Tags:       map[string]string{},
			// Carry the source's declaring file so the v2 emitter can map a
			// non-unique signature (notably "<pkg>.init", which repeats once per
			// file in a package) back to the specific callable that owns this
			// call site. See schema.GoCallEdge.SourcePath.
			SourcePath: callable.Path,
		})
	}

	// Closures: their own sites, then theirs in turn. Each carries its enclosing
	// callable's declaring file as Path, so the site index and the edge dedup
	// treat it like any other callable.
	for _, name := range sortedKeys(callable.InnerCallables) {
		inner, innerEdges := cg.resolveCallable(pkg, callable.InnerCallables[name], seen, knownSigs)
		callable.InnerCallables[name] = inner
		edges = append(edges, innerEdges...)
	}

	return callable, edges
}

// isLibrary reports whether fn lives outside the project: its declaring file is
// outside projectDir, or under a vendored directory within it. This does not depend on which packages were loaded as
// roots, so it also holds in incremental (-t) mode, where sibling project
// packages load only as dependencies. When the position is unavailable it falls
// back to comparing the import path against the project's module path.
func (cg *CallGraphBuilder) isLibrary(fn *types.Func) bool {
	if fn.Pkg() == nil {
		return false
	}
	if tf := cg.fset.File(fn.Pos()); tf != nil {
		file := absPath(tf.Name())
		root := cg.absProjectDir()
		if !utils.IsWithin(root, file) {
			return true
		}
		// A vendored dependency sits inside projectDir but is not project code
		// (the symbol table skips it), so it is a library too.
		return utils.IsVendored(utils.RelativePath(root, file))
	}
	if mod := cg.modulePath(); mod != "" {
		p := fn.Pkg().Path()
		return p != mod && !strings.HasPrefix(p, mod+"/")
	}
	_, inProject := cg.pkgs[fn.Pkg().Path()]
	return !inProject
}

func absPath(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

func (cg *CallGraphBuilder) absProjectDir() string { return absPath(cg.projectDir) }

// modulePath reads the project's module path from go.mod ("" when unavailable).
func (cg *CallGraphBuilder) modulePath() string {
	data, err := os.ReadFile(filepath.Join(cg.projectDir, "go.mod"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
			return strings.Trim(strings.TrimSpace(rest), "\"")
		}
	}
	return ""
}

// recordExternal registers a library callee, keyed by its full signature.
func (cg *CallGraphBuilder) recordExternal(fn *types.Func, fullSig string) {
	if _, ok := cg.externals[fullSig]; ok {
		return
	}
	if cg.externals == nil {
		cg.externals = map[string]schema.GoExternalCallee{}
	}
	cg.externals[fullSig] = ExternalCallee(fn, fullSig)
}

// ExternalCallee is the external_symbols entry of the library callee fn, whose
// full signature (CalleeSignatureOf) is fullSig. L2 and the L4 VTA pass both
// build their entries with it, so an external has one shape whichever found it.
// Parameter names and types come from the callee's *types.Signature; unnamed
// parameters become "_<i>". The receiver is not a parameter.
func ExternalCallee(fn *types.Func, fullSig string) schema.GoExternalCallee {
	importPath := fn.Pkg().Path()
	// Record the declared signature, not the instance the call site selected: a
	// method on a generic library type (atomic.Pointer[A].Store) must yield one
	// order-independent entry whose params mention the type parameter (*T).
	fn = fn.Origin()
	sig := fn.Type().(*types.Signature)

	var params []schema.GoParameter
	for i := 0; i < sig.Params().Len(); i++ {
		p := sig.Params().At(i)
		name := p.Name()
		if name == "" || name == "_" {
			name = fmt.Sprintf("_%d", i)
		}
		params = append(params, schema.GoParameter{
			Name:       name,
			Type:       types.TypeString(p.Type(), nil),
			IsVariadic: sig.Variadic() && i == sig.Params().Len()-1,
		})
	}

	var rets []string
	for i := 0; i < sig.Results().Len(); i++ {
		rets = append(rets, types.TypeString(sig.Results().At(i).Type(), nil))
	}
	ret := ""
	switch len(rets) {
	case 0:
	case 1:
		ret = rets[0]
	default:
		ret = "(" + strings.Join(rets, ", ") + ")"
	}

	var receiver *schema.GoParameter
	if recv := sig.Recv(); recv != nil {
		name := recv.Name()
		if name == "" || name == "_" {
			name = "_recv"
		}
		receiver = &schema.GoParameter{Name: name, Type: types.TypeString(recv.Type(), nil)}
	}

	return schema.GoExternalCallee{
		ImportPath: importPath,
		Sig:        strings.TrimPrefix(fullSig, importPath+"."),
		Name:       fn.Name(),
		Params:     params,
		ReturnType: ret,
		Receiver:   receiver,
	}
}

// resolveCallSite resolves a single call site to its callee function by looking
// it up in the package's call-site index under the key L1 recorded it with:
// relPath (the file declaring the enclosing callable) and the site's real
// (not //line-adjusted) start and end line:col. Returns nil when the site cannot be
// resolved (graceful fallback).
func (cg *CallGraphBuilder) resolveCallSite(pkg *packages.Package, relPath string, site *schema.GoCallsite) *types.Func {
	if pkg.TypesInfo == nil {
		return nil
	}
	key := callKey{relPath, site.StartLine, site.StartColumn, site.EndLine, site.EndColumn}
	call, ok := cg.indexFor(pkg)[key]
	if !ok {
		return nil
	}
	return cg.resolveCallExpr(pkg, call)
}

// indexFor returns the (lazily built, cached) call-site index of pkg.
func (cg *CallGraphBuilder) indexFor(pkg *packages.Package) callSiteIndex {
	if idx, ok := cg.indexes[pkg]; ok {
		return idx
	}
	idx := newCallSiteIndex(cg.projectDir, cg.fset, pkg)
	cg.indexes[pkg] = idx
	return idx
}

// resolveCallExpr resolves a *ast.CallExpr to its callee *types.Func using
// go/types. Type conversions, builtins and calls through function values are
// not calls to a function and resolve to nil.
func (cg *CallGraphBuilder) resolveCallExpr(pkg *packages.Package, call *ast.CallExpr) *types.Func {
	info := pkg.TypesInfo

	switch fn := call.Fun.(type) {
	case *ast.Ident:
		if f, ok := info.ObjectOf(fn).(*types.Func); ok {
			return f
		}
	case *ast.SelectorExpr:
		if sel, ok := info.Selections[fn]; ok {
			if f, ok := sel.Obj().(*types.Func); ok {
				return f
			}
		}
		// Package-level function via qualified identifier.
		if f, ok := info.ObjectOf(fn.Sel).(*types.Func); ok {
			return f
		}
	}
	return nil
}

// CalleeSignatureOf is the signature of a resolved callee, the call-site side
// of the one canonicalizer (syntactic_analysis.SignatureOf), so caller-side
// and callee-side ids are identical by construction. The L4 SSA join uses it
// for external callees (ruling R-P5).
func CalleeSignatureOf(obj types.Object) string {
	return syntactic_analysis.SignatureOf(obj)
}

// packageForFile returns the *packages.Package that contains the given relative file path.
func (cg *CallGraphBuilder) packageForFile(relPath string) *packages.Package {
	for _, pkg := range cg.pkgs {
		for _, f := range pkg.GoFiles {
			if utils.RelativePath(cg.projectDir, f) == relPath {
				return pkg
			}
		}
	}
	return nil
}

// MergeEdges merges two edge lists, unioning provenance and accumulating weight
// for duplicate edges. Mirrors Python's merge_edges(), with the edge identity
// (SourcePath, Source, Target, TargetPath). A signature is not unique (every
// file's func init shares "<pkg>.init", and so do its closures), so the
// declaring files keep two inits' edges apart.
func MergeEdges(primary, secondary []schema.GoCallEdge) []schema.GoCallEdge {
	type key struct{ path, src, tgt, tgtPath string }
	index := map[key]int{}
	result := make([]schema.GoCallEdge, 0, len(primary)+len(secondary))

	add := func(e schema.GoCallEdge) {
		k := key{e.SourcePath, e.Source, e.Target, e.TargetPath}
		if idx, exists := index[k]; exists {
			// Merge provenance (union) and accumulate weight.
			provSet := map[string]bool{}
			for _, p := range result[idx].Provenance {
				provSet[p] = true
			}
			for _, p := range e.Provenance {
				if !provSet[p] {
					result[idx].Provenance = append(result[idx].Provenance, p)
				}
			}
			result[idx].Weight += e.Weight
		} else {
			index[k] = len(result)
			result = append(result, e)
		}
	}

	for _, e := range primary {
		add(e)
	}
	for _, e := range secondary {
		add(e)
	}
	return result
}
