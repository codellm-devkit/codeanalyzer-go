package examples

// WrapStraight: MR8 wraps `x += a` in `if c { … }`; the def before it now also
// reaches the return (x += a is conditional).
func WrapStraight(a int) int {
	x := a
	x += a
	return x
}

// WrapInLoop: MR8 wraps `s += i` (a loop-body statement); the guard's def
// reaches its own condition around the loop (B→B on c, see conventions.go).
func WrapInLoop(n int) int {
	s := 0
	for i := 0; i < n; i++ {
		s += i
		s++
	}
	return s
}

// WrapBeforeReturn: MR8 wraps `y++`, the statement right before the return.
func WrapBeforeReturn(a int) int {
	y := a * 2
	y++
	return y
}
