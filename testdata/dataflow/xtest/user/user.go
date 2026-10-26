// Package user imports xtest from non-test code. With --skip-tests=false the
// loader gives it the plain xtest while xtest_test imports the test variant
// "xtest [xtest.test]": SSA then holds two packages for one import path.
package user

import "example.com/dataflow/xtest"

// Use calls into xtest.
func Use() int { return xtest.New().Add(3) }
