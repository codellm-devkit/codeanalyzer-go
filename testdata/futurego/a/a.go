//go:build go1.99

// Package a needs a Go newer than any toolchain. The go command excludes it
// unless the go1.99 tag is set (the test sets it through GOFLAGS); then
// go/types reports "file requires newer Go version go1.99", the error an
// analyzer built with an older Go hits on a module with a newer go line (a
// go.mod line of 1.99 itself is refused by the go command before any load).
package a

func A() int { return 1 }
