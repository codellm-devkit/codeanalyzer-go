// Package rangefuncpkg ranges over a function only inside a package-level
// function literal: that literal has no callable (it is no L3 unit), so the
// range_func entry must still list this package.
package rangefuncpkg

func seq(yield func(int) bool) {
	yield(1)
}

// Sum is a package-level variable holding a literal that ranges over seq.
var Sum = func() int {
	s := 0
	for v := range seq {
		s += v
	}
	return s
}
