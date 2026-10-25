package dataflow

import (
	"go/ast"
	"go/types"
	"sort"

	"github.com/codellm-devkit/codeanalyzer-go/internal/astwalk"
)

// What @entry defines (EntryBases) and the callable-level bases DefUseOf
// reads: named results and a function literal's captured free variables.

// namedResults returns fn's named result variables ("_" excluded).
func namedResults(fn ast.Node, info *types.Info) []types.Object {
	ft := funcType(fn)
	if ft == nil || ft.Results == nil {
		return nil
	}
	return fieldObjs(ft.Results, info)
}

func funcType(fn ast.Node) *ast.FuncType {
	switch fn := fn.(type) {
	case *ast.FuncDecl:
		return fn.Type
	case *ast.FuncLit:
		return fn.Type
	}
	return nil
}

// fieldObjs returns the objects of fl's named fields, in order ("_" and
// unnamed fields excluded).
func fieldObjs(fl *ast.FieldList, info *types.Info) []types.Object {
	if fl == nil {
		return nil
	}
	var out []types.Object
	for _, f := range fl.List {
		for _, name := range f.Names {
			if obj := info.Defs[name]; obj != nil && name.Name != "_" {
				out = append(out, obj)
			}
		}
	}
	return out
}

// EntryBases returns what @entry of callable fn (a *ast.FuncDecl or
// *ast.FuncLit) defines, in a deterministic order:
//
//  1. the receiver, the parameters and the named results in declaration
//     order, then — for a function literal — every captured free variable
//     (a variable declared outside the literal, not at package scope) by
//     position, then name;
//  2. every package-level variable fn's body reads or writes (not those of
//     nested function literals: those are their own callables), sorted by
//     package path, then name — positions of other files depend on load
//     order, so they are not used across files.
//
// "_" and unnamed parameters are skipped. A nil info yields nil.
func EntryBases(fn ast.Node, info *types.Info) []types.Object {
	if info == nil {
		return nil
	}
	var out []types.Object
	var body *ast.BlockStmt
	switch fn := fn.(type) {
	case *ast.FuncDecl:
		out = append(out, fieldObjs(fn.Recv, info)...)
		body = fn.Body
	case *ast.FuncLit:
		body = fn.Body
	default:
		return nil
	}
	if ft := funcType(fn); ft != nil {
		out = append(out, fieldObjs(ft.Params, info)...)
		out = append(out, fieldObjs(ft.Results, info)...)
	}
	if lit, ok := fn.(*ast.FuncLit); ok {
		out = append(out, freeVars(lit, info)...)
	}
	if body == nil {
		return out
	}
	seen := map[types.Object]bool{}
	var globals []types.Object
	ast.Inspect(body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.Ident:
			if obj := info.Uses[n]; isPkgVar(obj) && !seen[obj] {
				seen[obj] = true
				globals = append(globals, obj)
			}
		}
		return true
	})
	sort.Slice(globals, func(i, j int) bool {
		a, b := globals[i], globals[j]
		if a.Pkg().Path() != b.Pkg().Path() {
			return a.Pkg().Path() < b.Pkg().Path()
		}
		return a.Name() < b.Name()
	})
	return append(out, globals...)
}

// freeVars returns the variables function literal lit captures: every
// variable its body (nested literals included) uses that is declared outside
// lit and not at package scope, by position, then name.
func freeVars(lit *ast.FuncLit, info *types.Info) []types.Object {
	seen := map[types.Object]bool{}
	var out []types.Object
	ast.Inspect(lit.Body, func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if !ok {
			return true
		}
		v, ok := info.Uses[id].(*types.Var)
		if !ok || v.IsField() || isPkgVar(v) || seen[v] || (v.Pos() >= lit.Pos() && v.Pos() < lit.End()) {
			return true
		}
		seen[v] = true
		out = append(out, v)
		return true
	})
	sort.Slice(out, func(i, j int) bool {
		if out[i].Pos() != out[j].Pos() {
			return out[i].Pos() < out[j].Pos()
		}
		return out[i].Name() < out[j].Name()
	})
	return out
}

// facts are the callable-level inputs of DefUseOf's D1 rules (spec §4.5 "L3
// precision"), computed once per callable from its body.
type facts struct {
	// capW is CaptureWrites of each outermost function literal of the body.
	capW map[*ast.FuncLit][]Path
	// weakBases is the base of every path in W, the union of capW: a strong
	// def on one of them is weak (gap 1).
	weakBases map[types.Object]bool
	// deferOps maps an operand base of a registered defer to the "<defer>:"
	// pseudo paths of the defers it is an operand of, sorted (gap 3).
	deferOps map[types.Object][]string
}

// newFacts computes fn's callable-level facts; nil for a nil info or a
// callable without a body.
func newFacts(fn ast.Node, info *types.Info, k int, keyOf map[ast.Node]string) *facts {
	body := funcBody(fn)
	if info == nil || body == nil {
		return nil
	}
	f := &facts{capW: map[*ast.FuncLit][]Path{}, weakBases: map[types.Object]bool{}}
	for _, l := range astwalk.Literals(body) {
		ws := CaptureWrites(l.Lit, info, k)
		f.capW[l.Lit] = ws
		for _, p := range ws {
			f.weakBases[p.Base] = true
		}
	}
	f.deferOps = deferOperandBases(body, info, keyOf)
	return f
}

// CaptureWrites returns the paths function literal lit, or any literal nested
// in it, may write (strong or weak defs, by DefUseOf's def rules, k-limited),
// restricted to bases that are free variables of lit (freeVars: a variable
// declared inside lit, or at package scope, is not a capture). Sorted like a
// DefUse list (by spelling, then base position).
//
// A write is collected wherever it sits in lit's body: in a nested block, a
// loop or branch body, a deferred call (whose writes the deferred call node
// performs) or a nested literal's body.
func CaptureWrites(lit *ast.FuncLit, info *types.Info, k int) []Path {
	if lit == nil || info == nil {
		return nil
	}
	free := map[types.Object]bool{}
	for _, v := range freeVars(lit, info) {
		free[v] = true
	}
	if len(free) == 0 {
		return nil
	}
	c := &collector{info: info, k: k, seen: map[pathKind]bool{}}
	ast.Inspect(lit.Body, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.DeferStmt:
			c.stmt(s)
			c.callWrites(s.Call)
		case ast.Stmt:
			c.stmt(s)
		}
		return true
	})
	seen := map[Path]bool{}
	var out []Path
	for _, list := range [][]Path{c.du.Defs, c.du.WeakDefs} {
		for _, p := range list {
			if p.Base != nil && free[p.Base] && !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	sortPaths(out)
	return out
}
