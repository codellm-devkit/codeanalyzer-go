package generic

import "sync/atomic"

// A and B are two distinct instantiation arguments for atomic.Pointer.
type A struct{ n int }

var pa atomic.Pointer[A]

func StoreA() { pa.Store(nil) }
