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
