//go:build unix

package core

import (
	"io/fs"
	"syscall"
)

// currentUmask returns the process umask. The syscall can only set it, so it
// is set to 0 and restored at once, before the chmod that uses the result.
func currentUmask() fs.FileMode {
	m := syscall.Umask(0)
	syscall.Umask(m)
	return fs.FileMode(m)
}
