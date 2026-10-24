// Package xfile declares a type in a.go and a method on it in b.go; the
// method holds a closure and a nested closure (source_file, R19).
package xfile

// T is declared here; its method Run is declared in b.go.
type T struct{ n int }
