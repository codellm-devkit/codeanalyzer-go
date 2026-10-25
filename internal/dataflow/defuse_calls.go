package dataflow

import (
	"go/ast"
	"go/token"
	"go/types"
	"sort"
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

// callWrites collects the weak defs of call itself: a.* for each argument
// that may hold a reference (MayHoldRef: &x gives x.*), recv.* for a
// receiver that may hold one or is bound to a pointer-receiver method, and
// the element writes of append, copy, delete and clear. append(dst, …) may
// write into dst's backing array (spare capacity, or dst[:0]), so it
// weak-defines dst[*] besides its result flowing to the assignment.
// Conversions and other builtins write nothing.
func (c *collector) callWrites(call *ast.CallExpr) {
	if isType(call.Fun, c.info) {
		return
	}
	if name, ok := builtinName(call.Fun, c.info); ok {
		switch name {
		case "append", "copy", "delete", "clear":
			if len(call.Args) > 0 {
				x, _ := sliceBase(call.Args[0])
				if b, ok := pathOf(x, c.info); ok {
					c.addWeak(b.with("[*]"))
				}
			}
		}
		return
	}
	if sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr); ok {
		if s := c.info.Selections[sel]; s != nil && s.Kind() == types.MethodVal {
			if ptrRecv(s.Obj()) || c.holdsRef(c.info.TypeOf(sel.X)) {
				c.writeThrough(sel.X)
			}
		}
	}
	for _, a := range call.Args {
		if c.holdsRef(c.info.TypeOf(a)) {
			c.writeThrough(a)
		}
	}
}

// holdsRef is the call rule's write-through predicate: MayHoldRef, or the
// reference kinds alone (refTyped) for the pre-D1 gate sets.
func (c *collector) holdsRef(t types.Type) bool {
	if c.preD1 {
		return refTyped(t)
	}
	return MayHoldRef(t)
}

// writeThrough weak-defines a.* (x.* for &x) when a is a path, and base[*]
// when a slices a path (buf[:n], arr[:], (*p)[i:]): the callee writes the
// elements the slice shares with its base.
func (c *collector) writeThrough(a ast.Expr) {
	if base, sliced := sliceBase(a); sliced {
		if b, ok := pathOf(base, c.info); ok {
			c.addWeak(b.with("[*]"))
		}
		return
	}
	a = ast.Unparen(a)
	if u, ok := a.(*ast.UnaryExpr); ok && u.Op == token.AND {
		a = u.X
	}
	if b, ok := pathOf(a, c.info); ok {
		c.addWeak(b.with(".*"))
	}
}

// sliceBase strips slice expressions (and the parentheses, & and * between
// them) from e down to the sliced operand: buf[2:][:n] gives buf, (&arr)[:]
// and (*p)[:] give arr and p. sliced reports whether any slice expression was
// stripped; when it is false e is returned unchanged. The slice's elements are
// its base's elements, whether the base is a slice, an array or a pointer to
// an array, so a write through the slice is a write to base[*].
func sliceBase(e ast.Expr) (base ast.Expr, sliced bool) {
	base = e
	for {
		x := ast.Unparen(base)
		s, ok := x.(*ast.SliceExpr)
		if !ok {
			break
		}
		base, sliced = ast.Unparen(s.X), true
		switch b := base.(type) {
		case *ast.UnaryExpr:
			if b.Op == token.AND {
				base = b.X
			}
		case *ast.StarExpr:
			base = b.X
		}
	}
	if !sliced {
		return e, false
	}
	return base, true
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

// refTyped reports whether t is itself a reference kind: pointer, slice,
// map, chan, func or interface (a type parameter's underlying type is its
// constraint interface, so it counts).
func refTyped(t types.Type) bool {
	switch underlying(t).(type) {
	case *types.Pointer, *types.Slice, *types.Map, *types.Chan, *types.Signature, *types.Interface:
		return true
	}
	return false
}

// MayHoldRef reports whether a value of type t, passed by value, lets a
// callee write state the caller sees (spec §4.5 Calls, D1 gap 2): t is a
// reference kind (refTyped), or a struct or array any of whose fields or
// elements, transitively through named types, may hold one (a value
// cfg{items []int} shares items' backing array with its copy). A struct or
// array that holds no reference is copied whole and is false. Cycle-safe: a
// named type already on the walk is not re-entered (a value-only cycle,
// possible only in a package with type errors, holds nothing new). The call
// rule and the L4 by-ref formal_out rule share it.
func MayHoldRef(t types.Type) bool {
	return mayHoldRef(t, map[*types.Named]bool{})
}

func mayHoldRef(t types.Type, seen map[*types.Named]bool) bool {
	if t == nil {
		return false
	}
	if n, ok := types.Unalias(t).(*types.Named); ok {
		if seen[n] {
			return false
		}
		seen[n] = true
	}
	if refTyped(t) {
		return true
	}
	switch u := t.Underlying().(type) {
	case *types.Struct:
		for i := 0; i < u.NumFields(); i++ {
			if mayHoldRef(u.Field(i).Type(), seen) {
				return true
			}
		}
	case *types.Array:
		return mayHoldRef(u.Elem(), seen)
	}
	return false
}

// deferOperandBases maps each operand base of every `defer` in body (not in a
// nested function literal) to the defers' "<defer>:<call-key>" pseudo paths,
// sorted: the base of a method value's receiver and of each argument path
// (sliceBase and & stripped; a non-path argument has none). A function
// literal's captures are not operands: the deferred call node already uses
// them at exit. A defer keyOf does not name is skipped.
func deferOperandBases(body *ast.BlockStmt, info *types.Info, keyOf map[ast.Node]string) map[types.Object][]string {
	out := map[types.Object][]string{}
	add := func(e ast.Expr, spell string) {
		e, _ = sliceBase(e)
		e = ast.Unparen(e)
		if u, ok := e.(*ast.UnaryExpr); ok && u.Op == token.AND {
			e = u.X
		}
		b, ok := pathOf(e, info)
		if !ok || b.base == nil {
			return
		}
		for _, s := range out[b.base] {
			if s == spell {
				return
			}
		}
		out[b.base] = append(out[b.base], spell)
	}
	ast.Inspect(body, func(n ast.Node) bool {
		switch d := n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.DeferStmt:
			key, ok := keyOf[d.Call]
			if !ok {
				return true
			}
			spell := DeferPrefix + key
			if sel, ok := ast.Unparen(d.Call.Fun).(*ast.SelectorExpr); ok {
				if s := info.Selections[sel]; s != nil && s.Kind() == types.MethodVal {
					add(sel.X, spell)
				}
			}
			for _, a := range d.Call.Args {
				add(a, spell)
			}
		}
		return true
	})
	for _, ss := range out {
		sort.Strings(ss)
	}
	return out
}
