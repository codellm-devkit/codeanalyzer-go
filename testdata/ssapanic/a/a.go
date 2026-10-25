// Package a instantiates g.Map[int] and then calls Boom. A test hook panics
// on the call to Boom, after a's builder has created the shared instance.
package a

import "example.com/ssapanic/g"

func Boom() {}

func UseA() int {
	v := g.Map[int](1)
	Boom()
	return v
}
