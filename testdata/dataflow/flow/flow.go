// Package flow is the L3 fixture for the dataflow train: one named function per
// control-flow / dataflow construct. Tests pin body-node keys by line:col, so
// keep each statement on its own line and append new functions at the end.
package flow

func f(x int) int { return x }

func g(x int) int { return x }

func cleanup() {}

func work(x int) { _ = x }

// IfElse is an `if` with an init statement.
func IfElse(a int) int {
	if b := a * 2; b > 10 {
		return b
	} else {
		a = b
	}
	return a
}

// ForLoop is a 3-clause `for`.
func ForLoop(n int) int {
	s := 0
	for i := 0; i < n; i++ {
		s += i
	}
	return s
}

// Infinite is a `for {}` left only by a return.
func Infinite(ch chan int) {
	for {
		v := <-ch
		if v < 0 {
			return
		}
	}
}

// Range is a `for … range`.
func Range(xs []int) int {
	s := 0
	for _, x := range xs {
		s += x
	}
	return s
}

// Switch has a `fallthrough`.
func Switch(a int) int {
	r := 0
	switch a {
	case 1:
		r = 1
		fallthrough
	case 2:
		r += 2
	default:
		r = -1
	}
	return r
}

// TypeSwitch is a type switch.
func TypeSwitch(v any) string {
	switch t := v.(type) {
	case int:
		_ = t
		return "int"
	case string:
		return t
	default:
		return ""
	}
}

// Select2 selects over two channels.
func Select2(a, b chan int) int {
	select {
	case x := <-a:
		return x
	case b <- 1:
		return 0
	}
}

// Labeled has a labeled `break` and `continue`.
func Labeled(m [][]int) int {
	s := 0
outer:
	for _, row := range m {
		for _, v := range row {
			if v < 0 {
				continue outer
			}
			if v == 0 {
				break outer
			}
			s += v
		}
	}
	return s
}

// GotoBack is a backward `goto`.
func GotoBack(n int) int {
	i := 0
loop:
	i++
	if i < n {
		goto loop
	}
	return i
}

// EmptySelect blocks forever.
func EmptySelect() {
	select {}
}

// EarlyReturnDefer has two defers and an early return.
func EarlyReturnDefer(a int) int {
	defer cleanup()
	defer work(g(a))
	if a < 0 {
		return -1
	}
	cleanup()
	return a
}

// Panics has an explicit panic and a slice index.
func Panics(xs []int, i int) int {
	if i < 0 {
		panic("negative")
	}
	v := xs[i]
	return v
}

// Bump has the loop-carried y = y + 1.
func Bump(n int) int {
	y := 0
	for i := 0; i < n; i++ {
		y = y + 1
	}
	return y
}

// Shadow shadows x in an inner block.
func Shadow(a int) int {
	x := a
	if a > 0 {
		x := a * 2
		_ = x
	}
	return x
}

type H struct{ i int }

type G struct {
	h H
	x int
}

type F struct{ g G }

type P struct{ f F }

// Fields writes a deep access path, then reads a sibling path.
func Fields(p *P, v int) int {
	p.f.g.h.i = v
	return p.f.g.x
}

// Chan sends and receives on a channel.
func Chan(ch chan int, v int) int {
	ch <- v
	w := <-ch
	return w
}

// Unreachable has statements after a return.
func Unreachable(a int) int {
	return a
	a++
	return a + 1
}

// Nested is a call nested in a call inside an assignment.
func Nested(y int) int {
	x := f(g(y))
	return x
}

// Spawn launches a goroutine.
func Spawn(x int) {
	go work(f(x))
}

// get returns a *P so a statement can start with a call.
func get() *P { return &P{} }

// CallLHS is a statement whose first token starts a call: the assignment and
// the call share a position.
func CallLHS(v int) {
	get().f.g.x = v
}

// Closure has a function literal whose body is its own callable.
func Closure(x int) int {
	h := func() int {
		return f(x)
	}
	return h()
}

// S has chainable methods.
type S struct{}

// A returns s so calls can chain.
func (s *S) A() *S { return s }

// B ends the chain.
func (s *S) B() {}

// Chain is a bare call statement whose call is co-positioned with the call
// nested in it: s.A().B() is "l:c" (the statement's node), s.A() is "l:c/2".
func Chain(s *S) {
	s.A().B()
}

// SwitchNoDefault has no default; its fallthrough enters an empty clause.
func SwitchNoDefault(a int) int {
	switch a {
	case 1:
		a = 2
		fallthrough
	case 2:
	}
	return a
}

// BreakContinue has an unlabeled break and continue in a cond-only for.
func BreakContinue(n int) int {
	for n > 0 {
		n--
		if n == 5 {
			continue
		}
		if n == 2 {
			break
		}
	}
	return n
}

// GotoBlock jumps to a labeled block (target: its first statement) and to a
// labeled empty block (target: the block's continuation).
func GotoBlock(a int) int {
	if a > 0 {
		goto M
	}
	a--
M:
	{
		a++
	}
	if a > 5 {
		goto E
	}
	a = 0
E:
	{
	}
	return a
}

// GotoEnd jumps to a label on the implicit empty statement before `}`.
func GotoEnd(a int) {
	if a > 0 {
		goto L
	}
	a++
L:
}

// Spin has loops with empty bodies.
func Spin(n int) {
	for n > 0 {
	}
	for {
	}
}

// LabeledSwitchBreak leaves a labeled switch with `break Sw` from a nested for.
func LabeledSwitchBreak(a, n int) int {
Sw:
	switch a {
	case 1:
		for n > 0 {
			if n == 3 {
				break Sw
			}
			n--
		}
		a = n
	}
	return a
}

// LabeledSelectBreak leaves a labeled select with `break Sel`.
func LabeledSelectBreak(ch chan int, a int) int {
Sel:
	select {
	case v := <-ch:
		if v > 0 {
			break Sel
		}
		a = v
	}
	return a
}

// ContinueInSwitch continues the enclosing for from inside a switch.
func ContinueInSwitch(n int) int {
	for n > 0 {
		n--
		switch n {
		case 2:
			continue
		}
		n--
	}
	return n
}

// BreakInSelect breaks out of a select, not out of the enclosing for.
func BreakInSelect(ch chan int, n int) int {
	for n > 0 {
		select {
		case v := <-ch:
			if v == 0 {
				break
			}
			n = v
		}
		n--
	}
	return n
}

// DefaultMiddle has its default between two cases, with fallthrough across it.
func DefaultMiddle(a int) int {
	r := 0
	switch a {
	case 1:
		r = 1
		fallthrough
	default:
		r += 2
		fallthrough
	case 3:
		r += 3
	}
	return r
}

// TypeSwitchNil has a `case nil:` clause.
func TypeSwitchNil(v any) int {
	switch v.(type) {
	case nil:
		return 0
	case int:
		return 1
	}
	return 2
}

// GotoInLoop jumps backward inside a loop body.
func GotoInLoop(n int) int {
	for i := 0; i < n; i++ {
	again:
		n--
		if n > 10 {
			goto again
		}
	}
	return n
}

// GotoForever loops by a backward goto with no way out, under a defer.
func GotoForever(n int) {
	defer cleanup()
L:
	n++
	goto L
}

// SelectForever blocks in select {} under a defer.
func SelectForever() {
	defer cleanup()
	select {}
}

// PanicLast ends in a panic, so nothing falls off the end.
func PanicLast(xs []int, i int) {
	xs[i] = 1
	panic(i)
}

// ShadowPanic calls a local named panic: an ordinary call, not the builtin.
func ShadowPanic() int {
	panic := func(int) {}
	panic(1)
	return 2
}

// DeadDefer has a defer after a return: it is never registered.
func DeadDefer() {
	return
	defer cleanup()
}

// CondDefer registers a defer on one branch only.
func CondDefer(a int) int {
	if a > 0 {
		defer cleanup()
	}
	return a
}

// LoopDefer registers a defer on every iteration.
func LoopDefer(n int) {
	for i := 0; i < n; i++ {
		defer work(i)
	}
}

// LoopPanic panics in a nested block inside a loop.
func LoopPanic(xs []int) int {
	s := 0
	for _, x := range xs {
		if x < 0 {
			{
				panic(x)
			}
		}
		s += x
	}
	return s
}

// AfterPanic has statements, and a defer, after a panic.
func AfterPanic(a int) int {
	defer cleanup()
	panic(a)
	a++
	defer work(a)
	return a
}
