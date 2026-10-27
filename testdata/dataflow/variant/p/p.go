// Package p is the L4 fixture for whole-program SSA with tests loaded
// (--skip-tests=false): p_test.go makes go/packages yield the test variant
// "p [p.test]" beside the plain p that package u imports. Only the plain p's
// impl reaches u's interface call, so SSA must build the plain p too.
package p

// I is the interface u calls.
type I interface {
	M(x string) string
}

type impl struct{}

// Last is the global impl.M writes.
var Last string

// M records x and returns it.
func (impl) M(x string) string {
	Last = x
	return x
}

// New returns the one implementation.
func New() I {
	return impl{}
}
