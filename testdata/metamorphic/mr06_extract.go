package examples

// Extract: MR6 moves `x := a + 1; y := b * 2` into a helper taking a, b and
// returning x, y; a and b still reach the return through param_in/param_out.
func Extract(a, b int) int {
	x := a + 1
	y := b * 2
	return x + y
}
