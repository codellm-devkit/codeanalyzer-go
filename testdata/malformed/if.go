package malformed

// GoodIf is well formed.
func GoodIf(x int) int { return x + 1 }

func badIf() {
	if := 1
}
