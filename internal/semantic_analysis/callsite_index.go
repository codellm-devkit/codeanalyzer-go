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
// as real file positions (fset.PositionFor(p, false): //line directives are
// not applied, as L1 does not apply them). Start alone is ambiguous: a chained
// s.A().B() starts where its inner s.A() does.
type callKey struct {
	path                                    string
	startLine, startCol, endLine, endColumn int
}

// callSiteIndex maps a call expression's callKey to the call.
type callSiteIndex map[callKey]*ast.CallExpr

// newCallSiteIndex walks each file of pkg once and indexes every call. Real
// positions make the key unique per file (two calls never share both a start
// and an end); should one repeat, the first in source order wins.
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
			start, end := fset.PositionFor(call.Pos(), false), fset.PositionFor(call.End(), false)
			k := callKey{rel, start.Line, start.Column, end.Line, end.Column}
			if _, dup := idx[k]; !dup {
				idx[k] = call
			}
			return true
		})
	}
	return idx
}
