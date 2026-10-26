// Package storedlit is the L4 fixture for a global written only inside a
// stored function literal that runs later through a function value no
// analysis resolves (Task 13 review): inside Publish, v reaches sink's
// formal_out only through the caller's host, never through the literal.
package storedlit

import "sync/atomic"

// sink is written only by the literal Publish stores.
var sink string

var hook atomic.Value

// Publish stores a literal that writes v into sink.
func Publish(v string) {
	hook.Store(func() { sink = v })
}

// Fire calls the stored literal through an unresolved function value.
func Fire() {
	hook.Load().(func())()
}

func readSink() string { return sink }

// PublishThenRead returns what the stored literal wrote.
func PublishThenRead(v string) string {
	Publish(v)
	Fire()
	return readSink()
}
