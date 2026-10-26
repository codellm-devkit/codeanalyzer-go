// Package sdg is the L4 fixture for the dataflow train: one named function per
// interprocedural construct. Tests pin keys by line:col, so keep each
// statement on its own line and append new functions at the end.
package sdg

import (
	"bytes"
	"io"
)

// Store is an interface with two implementations.
type Store interface {
	Get(k string) string
}

type memStore struct {
	m map[string]string
}

func (s memStore) Get(k string) string {
	return s.m[k]
}

type sqlStore struct {
	dsn string
}

func (s *sqlStore) Get(k string) string {
	return s.dsn + k
}

// UseStore dispatches s.Get through the interface.
func UseStore(s Store, k string) string {
	return s.Get(k)
}

// Stores passes both implementations to UseStore, memStore also by pointer
// (its pointer wrapper maps to memStore.Get).
func Stores() {
	UseStore(memStore{}, "a")
	UseStore(&memStore{}, "b")
	UseStore(&sqlStore{}, "c")
}

func double(x int) int {
	return x * 2
}

// CallFuncValue calls f through a function value.
func CallFuncValue(f func(int) int) int {
	return f(1)
}

// PassDouble passes double to CallFuncValue.
func PassDouble() int {
	return CallFuncValue(double)
}

// Inner's method M is promoted to Outer.
type Inner struct {
	n int
}

func (i Inner) M() int {
	return i.n
}

// Outer embeds Inner.
type Outer struct {
	Inner
}

// Mer is satisfied by Outer and *Outer through the promoted M.
type Mer interface {
	M() int
}

// CallPromoted calls M through the interface.
func CallPromoted(o Mer) int {
	return o.M()
}

// PassOuter passes an Outer and an *Outer: the targets are the promotion
// wrapper and the promotion-and-pointer wrapper, both of Inner.M.
func PassOuter() int {
	a := CallPromoted(Outer{})
	b := CallPromoted(&Outer{})
	return a + b
}

// Buffered writes through an io.Writer.
func Buffered(w io.Writer, p []byte) {
	w.Write(p)
}

// PassBuffer passes a *bytes.Buffer: (*bytes.Buffer).Write is a target only
// VTA finds.
func PassBuffer() {
	Buffered(&bytes.Buffer{}, nil)
}

// Config is written only in initConfig and read only in readConfig, which
// recurses through useConfig: useConfig reads it transitively.
var Config string

// constOnly is initialized from a literal and never written: the global
// filter drops it.
var constOnly = 3

func initConfig() {
	Config = "on"
}

func readConfig() string {
	if Config == "" {
		return useConfig()
	}
	return Config
}

func useConfig() string {
	return readConfig()
}

func readConst() int {
	return constOnly
}

// mutate writes through p (a pointer) and s (a value holding a map); n is a
// value copy and q is never written through.
func mutate(p *Inner, s memStore, n Inner, q *Inner) {
	p.n = 1
	s.m["k"] = "v"
	n.n = 2
	_ = q
}

// setDSN writes through its pointer receiver.
func (s *sqlStore) setDSN(d string) {
	s.dsn = d
}
