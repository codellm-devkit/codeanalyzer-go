package xtest

import "testing"

// TestNew is an in-package test: it makes the loader yield the variant
// "example.com/dataflow/xtest [example.com/dataflow/xtest.test]", which has
// the plain package's path and shares its parsed a.go.
func TestNew(t *testing.T) {
	if New().Add(1) != 1 {
		t.Fatal("Add")
	}
}
