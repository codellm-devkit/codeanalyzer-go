package examples

// EarlyReturn: MR12 inserts `if c { return 0 }` before a statement; the
// statements after it become control dependent on the new branch.
func EarlyReturn(a int) int {
	x := a + 1
	return x
}

// EarlyReturnTwo: MR12 on a two-result function returns `0, ""` early.
func EarlyReturnTwo(a int) (int, string) {
	x := a * 3
	return x, "ok"
}
