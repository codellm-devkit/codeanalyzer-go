package generic

import "sync/atomic"

type B struct{ s string }

var pb atomic.Pointer[B]

func StoreB() { pb.Store(nil) }

// Ident is generic; Twice passes it its own type parameter, so SSA builds
// Twice's call through an instantiation wrapper of Ident (the type
// arguments are parameterized), which joins Ident via Origin().
func Ident[T any](v T) T { return v }

func Twice[T any](v T) T {
	f := Ident[T]
	return f(v)
}

// UseTwice instantiates Twice.
func UseTwice() int { return Twice(1) }
