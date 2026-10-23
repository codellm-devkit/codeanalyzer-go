package semantic_analysis

import (
	"go/ast"
	"go/token"

	"golang.org/x/tools/go/packages"
)

// callSiteIndex maps a call expression's start position to the call. Several
// calls can start at the same position (a chained s.A().B() starts where s.A()
// does); the outermost, first visited in pre-order, is kept, matching the
// first-match behaviour of the per-site AST walk this index replaces.
type callSiteIndex map[token.Pos]*ast.CallExpr

// newCallSiteIndex walks each file of pkg once and indexes every call by Pos().
func newCallSiteIndex(pkg *packages.Package) callSiteIndex {
	idx := callSiteIndex{}
	for _, f := range pkg.Syntax {
		ast.Inspect(f, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if _, dup := idx[call.Pos()]; !dup {
					idx[call.Pos()] = call
				}
			}
			return true
		})
	}
	return idx
}
