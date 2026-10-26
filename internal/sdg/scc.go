package sdg

import "slices"

// sccs returns the strongly connected components of the graph nodes and succ
// span (spec §4.9), callees first: a component comes after every component it
// reaches. Nodes only succ reaches are included. Each component is sorted by
// compareTarget.
//
// It is Tarjan's algorithm with an explicit stack, so a deep call chain
// cannot overflow the goroutine stack. The roots are visited in
// compareTarget order and each successor list is sorted and deduplicated
// first, so the result depends on neither the order of nodes nor the order
// succ returns.
func sccs(nodes []Target, succ func(Target) []Target) [][]Target {
	roots := slices.Clone(nodes)
	slices.SortFunc(roots, compareTarget)
	roots = slices.Compact(roots)

	type vertex struct {
		index, low int
		onStack    bool
	}
	type frame struct {
		v    Target
		succ []Target
		next int
	}
	state := make(map[Target]*vertex, len(roots))
	var stack []Target // Tarjan's stack
	var calls []frame  // the explicit DFS stack
	var out [][]Target

	visit := func(v Target) {
		n := len(state)
		state[v] = &vertex{index: n, low: n, onStack: true}
		stack = append(stack, v)
		s := slices.Clone(succ(v))
		slices.SortFunc(s, compareTarget)
		calls = append(calls, frame{v: v, succ: slices.Compact(s)})
	}
	for _, r := range roots {
		if state[r] != nil {
			continue
		}
		visit(r)
		for len(calls) > 0 {
			top := len(calls) - 1
			f := &calls[top]
			sv := state[f.v]
			if f.next < len(f.succ) {
				w := f.succ[f.next]
				f.next++
				switch sw := state[w]; {
				case sw == nil:
					visit(w)
				case sw.onStack:
					sv.low = min(sv.low, sw.index)
				}
				continue
			}
			v := f.v
			calls = calls[:top]
			if top > 0 {
				p := state[calls[top-1].v]
				p.low = min(p.low, sv.low)
			}
			if sv.low != sv.index {
				continue
			}
			i := len(stack) - 1
			for stack[i] != v {
				i--
			}
			comp := slices.Clone(stack[i:])
			stack = stack[:i]
			for _, w := range comp {
				state[w].onStack = false
			}
			slices.SortFunc(comp, compareTarget)
			out = append(out, comp)
		}
	}
	return out
}
