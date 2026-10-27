//go:build go1.98

// Package b needs go1.98: a second affected package, older than a's.
package b

func B() int { return 2 }
