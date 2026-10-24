package dataflow

import (
	"go/ast"
	"go/token"
	"go/types"
)

// The call rule of DefUseOf (spec §4.5), split into what a call reads and
// what it may write, because a `defer` evaluates the reads at registration
// and performs the writes in the exit chain.

// call collects a call evaluated and run at the same node.
func (c *collector) call(call *ast.CallExpr) {
	c.callReads(call, true)
	c.callWrites(call)
}

// callReads collects what evaluating call's operands reads: the function
// value (a method value's receiver, a func-typed variable, or — when
// captures is set — what a function literal captures) and the arguments,
// with the calls nested in them. A defer registration passes captures=false:
// a deferred closure reads its captures when it runs, at the exit.
func (c *collector) callReads(call *ast.CallExpr, captures bool) {
	if _, lit := ast.Unparen(call.Fun).(*ast.FuncLit); !lit || captures {
		c.use(call.Fun)
	}
	for _, a := range call.Args {
		c.use(a)
	}
}

// callWrites collects the weak defs of call itself: a.* for each
// reference-typed argument (&x gives x.*), recv.* for a reference-typed
// receiver or a pointer-receiver method, and the element writes of copy,
// delete and clear. Conversions and other builtins write nothing.
func (c *collector) callWrites(call *ast.CallExpr) {
	if isType(call.Fun, c.info) {
		return
	}
	if name, ok := builtinName(call.Fun, c.info); ok {
		switch name {
		case "copy", "delete", "clear":
			if len(call.Args) > 0 {
				if b, ok := pathOf(call.Args[0], c.info); ok {
					c.addWeak(b.with("[*]"))
				}
			}
		}
		return
	}
	if sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr); ok {
		if s := c.info.Selections[sel]; s != nil && s.Kind() == types.MethodVal {
			if ptrRecv(s.Obj()) || refTyped(c.info.TypeOf(sel.X)) {
				c.writeThrough(sel.X)
			}
		}
	}
	for _, a := range call.Args {
		if refTyped(c.info.TypeOf(a)) {
			c.writeThrough(a)
		}
	}
}

// writeThrough weak-defines a.* (x.* for &x) when a is a path.
func (c *collector) writeThrough(a ast.Expr) {
	a = ast.Unparen(a)
	if u, ok := a.(*ast.UnaryExpr); ok && u.Op == token.AND {
		a = u.X
	}
	if b, ok := pathOf(a, c.info); ok {
		c.addWeak(b.with(".*"))
	}
}

// builtinName returns the name of the builtin fun calls (panic, len, copy,
// unsafe.Sizeof, …), resolved through info so a shadowing local is not one.
func builtinName(fun ast.Expr, info *types.Info) (string, bool) {
	var id *ast.Ident
	switch f := ast.Unparen(fun).(type) {
	case *ast.Ident:
		id = f
	case *ast.SelectorExpr:
		id = f.Sel
	default:
		return "", false
	}
	b, ok := info.Uses[id].(*types.Builtin)
	if !ok {
		return "", false
	}
	return b.Name(), true
}

// ptrRecv reports whether method obj has a pointer receiver.
func ptrRecv(obj types.Object) bool {
	f, ok := obj.(*types.Func)
	if !ok {
		return false
	}
	recv := f.Signature().Recv()
	return recv != nil && isPointer(recv.Type())
}

func isPointer(t types.Type) bool {
	_, ok := underlying(t).(*types.Pointer)
	return ok
}

// refTyped reports whether a value of type t lets a callee write state the
// caller sees: pointer, slice, map, chan, func or interface (a type
// parameter's underlying type is its constraint interface, so it counts).
func refTyped(t types.Type) bool {
	switch underlying(t).(type) {
	case *types.Pointer, *types.Slice, *types.Map, *types.Chan, *types.Signature, *types.Interface:
		return true
	}
	return false
}
