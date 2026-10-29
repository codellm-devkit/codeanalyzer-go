package examples

// ConstScalar: MR13 replaces x in sinkInt(x) with 0; no formal_in reaches
// the call's actual_in any more.
func ConstScalar(a int) int {
	x := a + 1
	sinkInt(x)
	return x
}

// ConstTwoArgs: MR13 replaces x in sink2(x, 3) with 0.
func ConstTwoArgs(a int) int {
	x := a - 1
	sink2(x, 3)
	return x
}

// ConstInLoop: MR13 replaces x in the loop's sinkInt(x) with 0.
func ConstInLoop(a, n int) int {
	x := a * 2
	for i := 0; i < n; i++ {
		sinkInt(x)
	}
	return x
}
