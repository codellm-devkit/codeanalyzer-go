package examples

import "strconv"

// RouteIface: MR14(a) routes sinkInt(x) through a one-implementation
// interface (the literal rules out the closure form, and the call is not
// last, so not defer); the call's summary edge has its counterpart.
func RouteIface(a int) int {
	x := a + 1
	_ = func() {}
	sinkInt(x)
	return x
}

// RouteClosure: MR14(b) routes strconv.Itoa(x) through func(){ … }() (a
// library call: no interface form; not last: no defer).
func RouteClosure(a int) int {
	x := a + 1
	strconv.Itoa(x)
	return x
}

// RouteDefer: MR14(c) defers strconv.Itoa(x), the last statement.
func RouteDefer(a int) {
	x := a + 1
	_ = func() {}
	strconv.Itoa(x)
}
