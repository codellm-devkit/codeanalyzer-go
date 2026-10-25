package malformed

// GoodCase is well formed.
func GoodCase(x int) int { return x + 1 }

func badCase(x int) {
	switch x {
	case 1, :
	}
}
