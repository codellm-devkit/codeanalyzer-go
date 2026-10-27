package syntactic_analysis

import (
	"go/token"

	"golang.org/x/tools/go/packages"
)

// Fset returns the token.FileSet used during package loading.
// Used by CallGraphBuilder to resolve source positions.
func (b *SymbolTableBuilder) Fset() *token.FileSet { return b.fset }

// Pkgs returns the map of loaded packages keyed by package path.
// Used by CallGraphBuilder for type-info lookups.
func (b *SymbolTableBuilder) Pkgs() map[string]*packages.Package { return b.pkgs }

// Loaded returns every loaded package with types, in load order. Unlike
// Pkgs it keeps every variant of a package path: with tests loaded, both
// the test variant "p [p.test]" and the plain p other packages import.
// L4 builds SSA over all of them (sdg.Input.Variants).
func (b *SymbolTableBuilder) Loaded() []*packages.Package { return b.loaded }
