package malformed

// GoodRecv is well formed.
func GoodRecv(x int) int { return x + 1 }

func badRecv(c chan int) {
	select {
	case <-:
	}
}
