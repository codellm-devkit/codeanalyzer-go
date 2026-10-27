package xfile

// Run is declared apart from its receiver type T (a.go).
func (t *T) Run() int {
	add := func(d int) int {
		t.n += d
		inc := func() {
			t.n++
		}
		inc()
		return t.n
	}
	return add(1)
}
