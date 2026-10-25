// Package g declares a generic that packages a and b both instantiate at the
// same type argument, so they share one SSA instance.
package g

func Map[T any](x T) T { return x }
