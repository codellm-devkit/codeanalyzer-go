package examples

// pt is the struct MR5's alias writes go through.
type pt struct{ n int }

// SetPtr: MR5 rewrites p.n = v as `q := p; q.n = v`; v still reaches p's formal_out.
func SetPtr(p *pt, v int) {
	p.n = v
}

// SetValue: MR5 rewrites s.n = v as `q := &s; q.n = v`; v still reaches the return.
func SetValue(v int) int {
	var s pt
	s.n = v
	return s.n
}
