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

// aliasRange writes through s's elements via the loop variable, an alias.
func aliasRange(s []*Inner, v int) {
	for _, it := range s {
		it.n = v
	}
}

// aliasAssign writes through p via the local alias q.
func aliasAssign(p *Inner, v int) {
	q := p
	q.n = v
}

// aliasCall passes p's alias q to a callee.
func aliasCall(p *Inner) {
	q := p
	takeInner(q)
}

func takeInner(p *Inner) {}

// unusedRef never uses its by-ref param p.
func unusedRef(p *Inner, v int) int {
	return v
}

// multiT is a value receiver type that holds no reference.
type multiT struct{ n int }

// Multi has a receiver, a named and a blank param, and two results.
func (s multiT) Multi(a int, _ string) (int, error) {
	return a + s.n, nil
}

// Named has one named result: its formal_out is <return>, never n.
func Named() (n int) {
	n = 1
	return
}

// Variadic collapses excess actuals onto xs.
func Variadic(xs ...int) int {
	return len(xs)
}

func pair() (int, int) { return 1, 2 }

func takeTwo(a, b int) int { return a + b }

// CallVariadic passes three values, then pair's two results, to Variadic,
// and pair's results to takeTwo.
func CallVariadic() int {
	a := Variadic(1, 2, 3)
	b := Variadic(pair())
	c := takeTwo(pair())
	return a + b + c
}

// MethodValue calls s.Get through a bound method value.
func MethodValue(s *sqlStore, k string) string {
	f := s.Get
	return f(k)
}

// MethodExpr calls Get as a method expression: s binds the receiver.
func MethodExpr(s *sqlStore, k string) string {
	return (*sqlStore).Get(s, k)
}

// CaptureSite stores a literal that reads and writes the captured x.
func CaptureSite(v int) func() int {
	x := v
	f := func() int {
		x++
		return x
	}
	return f
}

// DirectLit calls a literal in place: a binds v, and the capture y binds at
// the call.
func DirectLit(v int) int {
	y := 0
	func(a int) {
		y = a
	}(v)
	return y
}

func upper(t string) string { return t }

// MixedValue calls a function value that is either a plain function or a
// bound method: only the method binds the function value as its receiver.
func MixedValue(s *sqlStore, b bool) string {
	f := upper
	if b {
		f = s.Get
	}
	return f("k")
}

// PrunedCall's second return is unreachable: its call node is pruned.
func PrunedCall(v int) int {
	return v
	return double(v)
}

// callUnknown calls a function value no caller supplies: unresolved.
func callUnknown(f func(int) int) int {
	return f(1)
}

// ChainLit starts a statement with a called literal whose result is
// written: the call holds the literal's line:col, the statement its /2.
func ChainLit(v int) {
	x := 0
	func() *Inner { _ = x; return &Inner{} }().n = v
}

func apply(f func() int) int { return f() }

// CaptureArg passes a capturing literal to a call nested in an assignment:
// the assignment, not the satellite call, hosts the capture site.
func CaptureArg(v int) int {
	n := apply(func() int { return v })
	return n
}

// DeadLit stores a capturing literal after a return, inside a kept branch:
// the literal is dead and has no capture site.
func DeadLit(v int) {
	if v > 0 {
		return
		_ = func() int { return v }
	}
}

type sumT struct{}

func (sumT) Sum(xs ...int) int { return len(xs) }

// MethodExprVariadic passes only the receiver to a variadic method
// expression: nothing reaches xs.
func MethodExprVariadic(v sumT) int {
	return sumT.Sum(v)
}

// CaptureNamed stores a literal that rewrites the named result err: the
// capture site's actual_out reaches err's formal_out (A7).
func CaptureNamed() (err error) {
	h := func() { err = nil }
	h()
	return
}

// CaptureParam stores a literal that writes through the by-ref param p: the
// capture site's actual_out reaches p's formal_out (A7).
func CaptureParam(p *Inner) {
	f := func() { p.n = 1 }
	f()
}

// a passes x to b, which passes it to c, which returns it: a's call of b
// has a summary edge from its actual_in:0 to its actual_out.
func a(x int) int {
	return b(x)
}

func b(x int) int {
	return c(x)
}

func c(x int) int {
	return x
}

// ignore never uses x: no summary edge leaves the actual_in:0 of a call
// of it.
func ignore(x, y int) int {
	return y
}

func callIgnore(v, w int) int {
	return ignore(v, w)
}

// dropX and dropY are mutually recursive and dropX never uses x: no summary
// edge leaves the actual_in:0 of dropY's call of dropX.
func dropX(x, y int) int {
	if y == 0 {
		return y
	}
	return dropY(y - 1)
}

func dropY(y int) int {
	return dropX(0, y)
}

// implicit's result depends on x only through control: x reaches
// <return> over the cdg.
func implicit(x int) int {
	if x > 0 {
		return 1
	}
	return 0
}

// Req is a request whose field a pointer copy writes (Task 17, §4.10).
type Req struct {
	UserID string
}

// AliasPtr writes req.UserID through the pointer copy p: the write reaches
// the return's read of req.UserID only through the may-alias oracle.
func AliasPtr(req *Req, id string) string {
	p := req
	p.UserID = id
	return req.UserID
}

// AliasPtrWhole is AliasPtr returning the whole pointer: the bare use req
// is prefix-compatible with the aliased write p.UserID.
func AliasPtrWhole(req *Req, id string) *Req {
	p := req
	p.UserID = id
	return req
}

// AliasValue copies req by value: the write to the copy never reaches req.
func AliasValue(req Req, id string) string {
	p := req
	p.UserID = id
	return req.UserID
}

// CallAliasAssign calls aliasAssign, whose v reaches its p only through the
// alias q: the call has a summary edge from v's actual_in to p's actual_out.
func CallAliasAssign(p *Inner, v int) {
	aliasAssign(p, v)
}

// AliasPtrValue takes req's address (spec §4.10's motivating case): the
// write through p reaches the read of the value req.
func AliasPtrValue(id string) string {
	var req Req
	p := &req
	p.UserID = id
	return req.UserID
}

// AliasArraySlice writes the array arr through its slice s.
func AliasArraySlice(v int) int {
	var arr [4]int
	s := arr[:]
	s[0] = v
	return arr[0]
}

// AliasFieldAddr writes o's embedded Inner through its address p: a
// one-step offset (p.n is o.Inner.n).
func AliasFieldAddr(v int) int {
	var o Outer
	p := &o.Inner
	p.n = v
	return o.Inner.n
}

// AliasValueCopies copies s twice by value, no address taken: the write to
// s1 never reaches s2 (Inner holds no reference).
func AliasValueCopies(s Inner, v int) int {
	s1 := s
	s2 := s
	s1.n = v
	return s2.n
}

// AliasRefField copies the map field s.m out: the write through m reaches
// the read through s.m (a reference-typed field, one-step offset).
func AliasRefField(s memStore, k, v string) string {
	m := s.m
	m[k] = v
	return s.m[k]
}

type ptrHolder struct {
	ptr *Inner
}

// AliasPtrField copies the pointer field r.ptr out: the write through p
// reaches the read through r.ptr.
func AliasPtrField(r ptrHolder, v int) int {
	p := r.ptr
	p.n = v
	return r.ptr.n
}

// AliasIfaces holds one value in two unrelated interfaces: the write
// through w may change what r reads.
func AliasIfaces(f *bytes.Buffer, p []byte) int {
	var r io.Reader = f
	var w io.Writer = f
	w.Write(p)
	n, _ := r.Read(p)
	return n
}

// AliasEntryValue: q may point to the value param req, so req's input
// reaches the read q.UserID through q (an alias edge from @entry whose def
// is req, not q).
func AliasEntryValue(req Req, q *Req, c bool) string {
	if c {
		q = &req
	}
	return q.UserID
}

// GReq is a value global whose address is taken (takeGReq).
var GReq Req

func takeGReq() *Req { return &GReq }

// AliasEntryGlobal reads GReq and q, which may point to GReq: GReq's input
// reaches the read q.UserID.
func AliasEntryGlobal(q *Req) string {
	s := GReq.UserID
	return s + q.UserID
}

// AliasPtrPtr rewrites p through its address pp: p.n then reads b.n.
func AliasPtrPtr(a, b *Inner) int {
	p := a
	pp := &p
	*pp = b
	return p.n
}

// AliasSlicePtr rewrites the slice s through its address ps.
func AliasSlicePtr(s, b []int) int {
	ps := &s
	*ps = b
	return s[0]
}

// AliasArrPtr writes the array pa points to through its slice s.
func AliasArrPtr(pa *[4]int) int {
	s := pa[:]
	s[0] = 1
	return pa[0]
}

// CaptureAlias stores a literal that writes through a local alias of the
// captured pointer p: the write is no def of p, but p may hold a reference,
// so the literal has a capture formal_out for p (the by-ref predicate) and
// x reaches p's formal_out (found by metamorphic relation MR5).
func CaptureAlias(p *Inner, x int) {
	f := func() { q := p; q.n = x }
	f()
}
