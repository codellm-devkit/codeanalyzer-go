package semantic_analysis

import (
	"go/ast"
	"go/token"

	"golang.org/x/tools/go/packages"

	"github.com/codellm-devkit/codeanalyzer-go/internal/utils"
)

// callKey identifies a call expression exactly as L1 records its site: the
// project-relative path of the file that contains it (the token.File name, the
// same path as the enclosing callable's Path) and its start and end line:col
// from fset.Position, which applies //line directives. Start alone is
// ambiguous: a chained s.A().B() starts where its inner s.A() does.
//
// The key is never turned back into a token.Pos: under a //line directive the
// recorded line names a virtual line (often past the file's real line count),
// so rebuilding a position from it would be wrong or panic.
type callKey struct {
	path                                    string
	startLine, startCol, endLine, endColumn int
}

// callSiteIndex maps a call expression's callKey to the call.
type callSiteIndex map[callKey]*ast.CallExpr

// newCallSiteIndex walks each file of pkg once and indexes every call. When
// two calls of one file share a key (only possible when //line directives map
// both to the same virtual range), the first in source order wins.
func newCallSiteIndex(projectDir string, fset *token.FileSet, pkg *packages.Package) callSiteIndex {
	idx := callSiteIndex{}
	for _, f := range pkg.Syntax {
		tf := fset.File(f.Pos())
		if tf == nil {
			continue
		}
		rel := utils.RelativePath(projectDir, tf.Name())
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			start, end := fset.Position(call.Pos()), fset.Position(call.End())
			k := callKey{rel, start.Line, start.Column, end.Line, end.Column}
			if _, dup := idx[k]; !dup {
				idx[k] = call
			}
			return true
		})
	}
	return idx
}
