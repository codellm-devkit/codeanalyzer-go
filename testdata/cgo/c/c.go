// Package c has a cgo file. go/packages parses the cgo-generated copy of it
// (in the build cache) instead, so its functions have no symbol-table
// callable (#29) and their SSA functions are unmapped_ssa.
package c

// int twice(int x) { return 2 * x; }
import "C"

// Twice calls C.
func Twice(x int) int { return int(C.twice(C.int(x))) }
