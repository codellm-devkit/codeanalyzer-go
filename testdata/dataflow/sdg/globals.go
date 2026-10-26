package sdg

// The globals of the L4 footprint fixture that need the os import; they live
// apart from sdg.go so that its line:col keys stay put.

import "os"

// home's initializer calls a function: the global filter keeps it.
var home = os.Getenv("HOME")

// counter is written only through its address: the global filter keeps it.
var counter int

// readArgs reads os.Args, a non-project package variable.
func readArgs() []string {
	return os.Args
}

func readHome() string {
	return home
}

func bump() {
	p := &counter
	*p = *p + 1
}

func readCounter() int {
	return counter
}
