//go:build !unix

package core

import "io/fs"

// currentUmask returns 0 where the platform has no umask (Windows): the file
// gets mode 0666, which os.Create would also give it.
func currentUmask() fs.FileMode { return 0 }
