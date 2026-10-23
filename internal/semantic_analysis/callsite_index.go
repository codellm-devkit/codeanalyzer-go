package semantic_analysis

import (
	"go/ast"
	"go/token"

	"golang.org/x/tools/go/packages"
)

// callKey identifies a call expression by its start AND end position. Start
// alone is ambiguous: a chained s.A().B() starts where its inner s.A() does.
type callKey struct{ pos, end token.Pos }

// callSiteIndex maps a call expression's [Pos, End) to the call.
type callSiteIndex map[callKey]*ast.CallExpr

// newCallSiteIndex walks each file of pkg once and indexes every call.
func newCallSiteIndex(pkg *packages.Package) callSiteIndex {
	idx := callSiteIndex{}
	for _, f := range pkg.Syntax {
		ast.Inspect(f, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				idx[callKey{call.Pos(), call.End()}] = call
			}
			return true
		})
	}
	return idx
}
