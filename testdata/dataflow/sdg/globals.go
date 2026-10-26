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

// g1 and g2 are read by skipX and skipY; tests force skipX to have no L3
// result.
var g1 = os.Getenv("G1")

var g2 = os.Getenv("G2")

func skipX() string {
	return g1 + skipY()
}

func skipY() string {
	return g2
}

func callsSkipX() string {
	return skipX()
}

// lazyOnce stands for library code that runs a callback later: Do keeps f
// and never calls it, so no call edge reaches Get's literal.
type lazyOnce struct{ f func() }

func (o *lazyOnce) Do(f func()) { o.f = f }

var once lazyOnce

var inst *Inner

func build() *Inner { return &Inner{} }

// Get writes inst only inside the literal it hands to once.Do.
func Get() *Inner {
	once.Do(func() { inst = build() })
	return inst
}

func callsGet() *Inner { return Get() }

// argv is initialized from a library variable, cfgDir from a kept global:
// the chained initializer rule keeps both.
var argv = os.Args

var cfgDir = home + "/x"

func readArgv() []string { return argv }

func readCfgDir() string { return cfgDir }

// litG and litH are written only by literals called where they are written.
var litG, litH int

// calledLits writes litG and litH only inside literals it calls in place
// (one deferred): with no VTA, no call edge reaches them.
func calledLits() {
	defer func() { litH = 2 }()
	func() { litG = 1 }()
}
