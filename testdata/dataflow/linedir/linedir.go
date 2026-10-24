// Package linedir has calls under a //line directive, the shape of generated
// code (goyacc, templ): L1 records them at their virtual positions, far past
// the file's real line count, and L2 must still resolve them.
package linedir

import "os"

func helper() string { return "" }

// Caller's calls sit on virtual lines of virtual.y.
func Caller() {
//line virtual.y:900:1
	os.Getenv(helper())
//line linedir.go:15:1
	helper()
}
