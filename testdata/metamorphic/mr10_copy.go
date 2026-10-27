package examples

// CopyInReturn: MR10 inserts `y := x` before the return and returns y*2;
// x's def now reaches the copy, the copy the return.
func CopyInReturn(a int) int {
	x := a + 1
	return x * 2
}

// CopyInAssign: MR10 copies one of the reads of the assignment's right side.
func CopyInAssign(a, b int) int {
	z := a - b
	return z
}
