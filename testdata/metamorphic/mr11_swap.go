package examples

// Swap: MR11 swaps the two independent definitions; ddg and cdg are unchanged.
func Swap(a, b int) int {
	x := a + 1
	y := b + 2
	return x + y
}
