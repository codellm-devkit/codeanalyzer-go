// Package vend calls into the vendored dependency vlib.
package vend

import "example.com/vlib"

func CallVendored() int { return vlib.F() }

func CallUnnamed() bool { return vlib.G(1, "a") && vlib.H(2, "b") != "" }
