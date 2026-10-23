// Package vend calls into the vendored dependency vlib.
package vend

import "example.com/vlib"

func CallVendored() int { return vlib.F() }
