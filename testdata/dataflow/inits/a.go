// Package inits has a func init() in more than one file. Every init shares the
// signature example.com/dataflow/inits.init, but each is its own callable.
package inits

import "os"

var mode string

func init() {
	setup()
	mode = os.Getenv("INITS_MODE")
}

func setup() { mode = "default" }
