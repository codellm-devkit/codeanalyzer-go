// Package l1 is the L1/L2 fixture for the dataflow train: type kinds, defer/go
// markers, and co-positioned (chained) calls.
package l1

// Celsius is a defined type over a non-struct.
type Celsius float64

// Alias is a type alias.
type Alias = Celsius

// Base is a plain struct.
type Base struct{ N int }

// MyErr is defined over a struct, so its kind is struct (underlying type).
type MyErr Base

// I1 is an interface.
type I1 interface{ M() }

// I2 is defined over an interface, so its kind is interface (underlying type).
type I2 I1

// S is a struct whose methods chain.
type S struct{}

// A returns a *S so calls can be chained.
func (s *S) A() *S { return s }

// B terminates the chain.
func (s *S) B() {}

func cleanup() {}

func work() {}

// D exercises defer and go call markers.
func D() {
	defer cleanup()
	go work()
}

// Chain has two calls that start at the same line and column.
func Chain(s *S) {
	s.A().B()
}
