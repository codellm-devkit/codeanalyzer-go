// Package semantic_analysis builds the resolver-based call graph (Level 1, Tier 1).
//
// This stage uses the same golang.org/x/tools/go/packages load that built the
// symbol table. For each recorded call site it resolves the callee to a
// *types.Func, derives its signature via signatureOf(), backfills
// callee_signature in place, and emits an identity-only GoCallEdge.
//
// Precision choice: declared-type dispatch (CHA-style). Pointer-receiver
// methods are followed; interface dispatch records the interface method
// signature and falls back gracefully when the concrete type is unknown.
package semantic_analysis

import (
	"go/ast"
	"go/token"
	"go/types"

	"golang.org/x/tools/go/packages"

	"github.com/codellm-devkit/codeanalyzer-go/internal/schema"
	"github.com/codellm-devkit/codeanalyzer-go/internal/utils"
)

// CallGraphBuilder resolves call sites and builds the call graph.
type CallGraphBuilder struct {
	projectDir string
	fset       *token.FileSet
	pkgs       map[string]*packages.Package
}

// NewCallGraphBuilder creates a builder using the same pkgs/fset loaded for the symbol table.
func NewCallGraphBuilder(projectDir string, fset *token.FileSet, pkgs map[string]*packages.Package) *CallGraphBuilder {
	return &CallGraphBuilder{projectDir: projectDir, fset: fset, pkgs: pkgs}
}

// Build resolves all call sites in symbolTable, backfills callee_signature, and
// returns the identity-only edge list. Never crashes on unresolved sites — it logs
// and skips the edge while leaving callee_signature nil.
func (cg *CallGraphBuilder) Build(symbolTable map[string]schema.GoFile) []schema.GoCallEdge {
	var edges []schema.GoCallEdge
	seen := map[[2]string]bool{}

	// Build the known-sig set once. Only emit edges where the target is in the
	// project's symbol table (drop stdlib / external callees, same as Python/Jedi).
	knownSigs := buildKnownSigs(symbolTable)

	for fileKey, goFile := range symbolTable {
		// Find the package for this file.
		pkg := cg.packageForFile(fileKey)
		if pkg == nil || pkg.TypesInfo == nil {
			continue
		}

		// Resolve methods on types.
		for typeSig, goType := range goFile.Types {
			for methSig, callable := range goType.Methods {
				newCallable, newEdges := cg.resolveCallable(pkg, callable, seen, knownSigs)
				goType.Methods[methSig] = newCallable
				edges = append(edges, newEdges...)
			}
			goFile.Types[typeSig] = goType
		}

		// Resolve package-level functions.
		for fnSig, callable := range goFile.Functions {
			newCallable, newEdges := cg.resolveCallable(pkg, callable, seen, knownSigs)
			goFile.Functions[fnSig] = newCallable
			edges = append(edges, newEdges...)
		}

		symbolTable[fileKey] = goFile
	}

	return edges
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
// Only emits edges where the target is in knownSigs (project-internal callees).
// External/stdlib callees have callee_signature backfilled but no edge emitted —
// matching Python/Jedi's behavior of dropping unresolved external sites.
func (cg *CallGraphBuilder) resolveCallable(
	pkg *packages.Package,
	callable schema.GoCallable,
	seen map[[2]string]bool,
	knownSigs map[string]bool,
) (schema.GoCallable, []schema.GoCallEdge) {
	var edges []schema.GoCallEdge

	for i := range callable.CallSites {
		site := &callable.CallSites[i]
		if site.CalleeSignature != nil {
			continue // already resolved
		}

		calleeSig := cg.resolveCallSite(pkg, site)
		if calleeSig == "" {
			utils.Debug("unresolved call site: %s in %s", site.MethodName, callable.Signature)
			continue
		}

		// Backfill the site regardless of whether the callee is in-project.
		site.CalleeSignature = &calleeSig

		// Only emit an edge when the callee is in the project's symbol table.
		if !knownSigs[calleeSig] {
			utils.Debug("external callee (no edge): %s", calleeSig)
			continue
		}

		key := [2]string{callable.Signature, calleeSig}
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
		})
	}

	return callable, edges
}

// resolveCallSite resolves a single call site to a callee signature string.
// Returns "" when the site cannot be resolved (graceful fallback).
func (cg *CallGraphBuilder) resolveCallSite(pkg *packages.Package, site *schema.GoCallsite) string {
	if pkg.TypesInfo == nil {
		return ""
	}

	// Walk the package's syntax to find the call expression at this location.
	for _, astFile := range pkg.Syntax {
		var result string
		ast.Inspect(astFile, func(n ast.Node) bool {
			if result != "" {
				return false
			}
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			pos := cg.fset.Position(call.Pos())
			if pos.Line != site.StartLine || pos.Column != site.StartColumn {
				return true
			}
			result = cg.resolveCallExpr(pkg, call)
			return false
		})
		if result != "" {
			return result
		}
	}
	return ""
}

// resolveCallExpr resolves a *ast.CallExpr to a callee signature using go/types.
func (cg *CallGraphBuilder) resolveCallExpr(pkg *packages.Package, call *ast.CallExpr) string {
	info := pkg.TypesInfo

	switch fn := call.Fun.(type) {
	case *ast.Ident:
		if obj := info.ObjectOf(fn); obj != nil {
			if f, ok := obj.(*types.Func); ok {
				return calleeSignatureOf(f)
			}
			// Type conversion — normalize as constructor.
			if tn, ok := obj.(*types.TypeName); ok {
				return calleeSignatureOf(tn) + ".__new__"
			}
		}
	case *ast.SelectorExpr:
		sel, ok := info.Selections[fn]
		if ok {
			if f, ok := sel.Obj().(*types.Func); ok {
				return calleeSignatureOf(f)
			}
		}
		// Package-level function via qualified identifier.
		if obj := info.ObjectOf(fn.Sel); obj != nil {
			if f, ok := obj.(*types.Func); ok {
				return calleeSignatureOf(f)
			}
		}
	}
	return ""
}

// calleeSignatureOf is the call-site mirror of signatureOf — same canonicalization.
// Must produce byte-identical strings to signatureOf() in syntactic_analysis.
func calleeSignatureOf(obj types.Object) string {
	if obj == nil {
		return ""
	}
	pkg := obj.Pkg()
	pkgPath := ""
	if pkg != nil {
		pkgPath = pkg.Path()
	}

	switch o := obj.(type) {
	case *types.Func:
		sig := o.Type().(*types.Signature)
		recv := sig.Recv()
		if recv != nil {
			recvType := recv.Type()
			if ptr, ok := recvType.(*types.Pointer); ok {
				recvType = ptr.Elem()
			}
			if named, ok := recvType.(*types.Named); ok {
				typeName := named.Obj().Name()
				return pkgPath + "." + typeName + "." + o.Name()
			}
		}
		return pkgPath + "." + o.Name()
	case *types.TypeName:
		return pkgPath + "." + o.Name()
	default:
		return pkgPath + "." + obj.Name()
	}
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
// for duplicate (source, target) pairs. Mirrors Python's merge_edges().
func MergeEdges(primary, secondary []schema.GoCallEdge) []schema.GoCallEdge {
	type key struct{ src, tgt string }
	index := map[key]int{}
	result := make([]schema.GoCallEdge, 0, len(primary)+len(secondary))

	add := func(e schema.GoCallEdge) {
		k := key{e.Source, e.Target}
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
