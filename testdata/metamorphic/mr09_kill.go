package examples

// KillOneUse: MR9 inserts `x = 0` before sinkInt(x); x := a + 1 no longer
// reaches the call or the return, the kill does.
func KillOneUse(a int) int {
	x := a + 1
	sinkInt(x)
	return x
}

// KillTwoUses: MR9 inserts `x = 0` before the return, which reads x twice.
func KillTwoUses(a int) int {
	x := a + 1
	return x * x
}

// KillNamed: MR9 inserts `n = 0` before the bare return; the kill takes over
// n's port edge into @formal_out (see conventions.go).
func KillNamed(a int) (n int) {
	n = a + 1
	return
}
