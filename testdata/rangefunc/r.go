// Package rangefunc holds the Go 1.23 range forms (#28): a range over an
// integer and a range over an iterator function. testdata/dataflow declares
// go 1.21, which rejects both.
package rangefunc

// RangeInt ranges over an integer: the header neither calls nor panics.
func RangeInt() int {
	s := 0
	for i := range 10 {
		s += i
	}
	return s
}

// RangeFunc ranges over an iterator function: the header calls seq, so it
// may panic.
func RangeFunc(seq func(func(int) bool)) int {
	s := 0
	for v := range seq {
		s += v
	}
	return s
}

func sink(int) {}

// RangeFuncCall calls sink in the body of a range over an iterator: in SSA
// the call sits in the synthetic yield function, which folds into
// RangeFuncCall.
func RangeFuncCall(seq func(func(int) bool)) {
	for v := range seq {
		sink(v)
	}
}
