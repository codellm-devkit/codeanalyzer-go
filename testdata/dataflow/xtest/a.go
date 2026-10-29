// Package xtest is called from an external test package (a_test.go, package
// xtest_test) and from an in-package test (b_test.go). With
// --skip-tests=false the loader also yields the test variant
// "xtest [xtest.test]"; package user imports the plain xtest, so SSA holds
// both. The join must map each variant's functions to its one callable.
//
// It also holds one function per SSA wrapper kind the join unwraps: a method
// value ($bound), a method expression ($thunk), a promoted method and a
// pointer wrapper of a value method.
package xtest

// Counter counts.
type Counter struct{ n int }

// Add adds d through a closure.
func (c *Counter) Add(d int) int {
	inc := func() { c.n += d }
	inc()
	return c.n
}

// Get is a value method: (*Counter).Get is its pointer wrapper.
func (c Counter) Get() int { return c.n }

// Outer promotes Counter's methods.
type Outer struct{ *Counter }

// New returns a counter.
func New() *Counter { return &Counter{} }

// Bound returns a method value: SSA makes the closure (*Counter).Add$bound.
func Bound(c *Counter) func(int) int { return c.Add }

// Thunk returns a method expression: SSA makes (*Counter).Add$thunk.
func Thunk() func(*Counter, int) int { return (*Counter).Add }

// Adder is an interface: a method value of it is a $bound wrapper of the
// interface method, and a method expression a $thunk of it.
type Adder interface{ Add(int) int }

// IfaceBound returns a method value of an interface.
func IfaceBound(a Adder) func(int) int { return a.Add }

// IfaceThunk returns a method expression of an interface.
func IfaceThunk() func(Adder, int) int { return Adder.Add }
