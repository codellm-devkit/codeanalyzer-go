package generic

import "sync/atomic"

type B struct{ s string }

var pb atomic.Pointer[B]

func StoreB() { pb.Store(nil) }
