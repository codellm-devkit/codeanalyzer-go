package fn

// Ordered is a union-constraint interface for comparable ordered primitives.
type Ordered interface {
	~int | ~int64 | ~float64 | ~string
}

// Numeric extends Ordered with unsigned integer kinds.
type Numeric interface {
	Ordered
	~uint | ~uint64
}

func Min[T Ordered](a, b T) T {
	if a < b {
		return a
	}
	return b
}

func Max[T Ordered](a, b T) T {
	if a > b {
		return a
	}
	return b
}

// Map applies f to every element of in and returns the results.
func Map[T, U any](in []T, f func(T) U) []U {
	out := make([]U, len(in))
	for i, v := range in {
		out[i] = f(v)
	}
	return out
}

// Filter returns the elements of in for which keep returns true.
func Filter[T any](in []T, keep func(T) bool) []T {
	var out []T
	for _, v := range in {
		if keep(v) {
			out = append(out, v)
		}
	}
	return out
}
