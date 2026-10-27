// Package vlib is a vendored dependency of the dataflow fixture.
package vlib

// F is a library function reached through the vendor directory.
func F() int { return 1 }

// G has unnamed parameters: its external symbol names them _0, _1.
func G(int, string) bool { return true }

// H has a blank parameter: it is named by position too (_0), n keeps its name.
func H(_ int, n string) string { return n }
