package examples

// The oracle conventions internal/metamorphic encodes; each is pinned by a
// test there (TestConvention_*), which names the rule when it breaks.

// ConvCaptureSite (i): the literal's capture site actual_in for p feeds only
// the closure's formal_in (param_in), never a summary or ddg edge: it is not
// a reachability sink.
func ConvCaptureSite(p *pt, v int) {
	f := func() { p.n = v }
	f()
}

// ConvGuardInLoop (ii): MR8 wrapping `s += i` in a loop body gives the guard
// the self-loop B→B on c.
func ConvGuardInLoop(n int) int {
	s := 0
	for i := 0; i < n; i++ {
		s += i
		s++
	}
	return s
}

// ConvGuardStraight (ii): MR8 wrapping `x += a` outside a loop gives no
// guard edge.
func ConvGuardStraight(a int) int {
	x := a
	x += a
	return x
}

// ConvDiscardedResult (iii): p.sum()'s discarded result reaches the return
// only through actual_out → call node (coarse wiring) and the call node's
// control dependence (it may panic): MR14(b) cuts that actual_out.
func ConvDiscardedResult(p pt2) int {
	p.sum()
	return p.a
}

// pt2 has a value method.
type pt2 struct{ a, b int }

func (p pt2) sum() int { return p.a + p.b }

// ConvNamedKill (iv): `n = a + 1` reaches @formal_out through the @exit
// port edge; MR9's kill before the return takes it over.
func ConvNamedKill(a int) (n int) {
	n = a + 1
	return
}
