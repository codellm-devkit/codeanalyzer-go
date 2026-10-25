package malformed

// GoodRange is well formed.
func GoodRange(x int) int { return x + 1 }

func badRange() {
	for range {
	}
}
