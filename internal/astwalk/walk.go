// Package astwalk is the one call-site / closure walker of a callable body,
// shared by L1 (the symbol-table builder records call sites and names
// closures with it) and L3/L4 (the dataflow body builder pairs its call nodes
// with those sites). One walk, so the two levels can never drift apart. It
// imports only the standard library, so any package may use it.
package astwalk

import (
	"go/ast"
	"strconv"
)

// Call is one call expression of a body, with the go / defer statement flag
// of the statement whose own call it is.
type Call struct {
	Expr      *ast.CallExpr
	Goroutine bool // the call of a `go` statement
	Deferred  bool // the call of a `defer` statement
}

// Calls lists body's call expressions in L1 recording order: pre-order — an
// outer call before the calls nested in it, a `go` / `defer` statement's call
// before the calls in its function value and arguments — and no function
// literal bodies (each literal is its own callable; see Literals). A nil body
// has no calls.
func Calls(body *ast.BlockStmt) []Call {
	if body == nil {
		return nil
	}
	var out []Call
	goroutine := map[*ast.CallExpr]bool{}
	deferred := map[*ast.CallExpr]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.GoStmt:
			goroutine[n.Call] = true
		case *ast.DeferStmt:
			deferred[n.Call] = true
		case *ast.FuncLit:
			return false
		case *ast.CallExpr:
			out = append(out, Call{Expr: n, Goroutine: goroutine[n], Deferred: deferred[n]})
		}
		return true
	})
	return out
}

// Literal is one outermost function literal of a body and its closure name.
type Literal struct {
	Lit  *ast.FuncLit
	Name string // closure_1, closure_2, …
}

// Literals lists the outermost function literals of body in ast.Inspect
// order, named closure_1, closure_2, …. A literal nested in another is not
// listed: it is the enclosing literal's own (Literals(lit.Body)). A nil body
// has none.
func Literals(body *ast.BlockStmt) []Literal {
	if body == nil {
		return nil
	}
	var out []Literal
	ast.Inspect(body, func(n ast.Node) bool {
		lit, ok := n.(*ast.FuncLit)
		if !ok {
			return true
		}
		out = append(out, Literal{Lit: lit, Name: "closure_" + strconv.Itoa(len(out)+1)})
		return false
	})
	return out
}
