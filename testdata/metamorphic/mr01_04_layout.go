package examples

// MR1-MR4, MR7, MR15 (layout, rename, target-files): a type with a method
// declared above it, a function, and locals to rename. Exact oracle: the
// analysis is unchanged up to positions (and spelling for MR4).

// Scale is declared above its receiver type (MR2 reverses it back below).
func (c *counter) Scale(k int) int {
	m := c.n * k
	return m
}

// counter is a struct with one field.
type counter struct{ n int }

// Sum adds two values through locals (MR3 may move it; MR4 renames a, b, s).
func Sum(a, b int) int {
	s := a + b
	return s
}
