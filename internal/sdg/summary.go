package sdg

import (
	"math/bits"
	"slices"
	"strings"

	"github.com/codellm-devkit/codeanalyzer-go/internal/dataflow"
)

// sdgBuilder computes the flow summaries (spec §4.9) and the L4 overlay of
// a run over one ports. Like ports, it is not safe for concurrent use.
type sdgBuilder struct {
	in    Input
	d     *dispatch
	p     *ports
	flows map[Target]*flowSet
	info  map[Target]*formalInfo
	sites map[dataflow.L3Key][]siteFlow
	graph map[dataflow.L3Key]*unitGraph
	wired map[dataflow.L3Key][]PortEdge
	// summ is each unit's summary edges, produced as soon as its SCC is
	// solved (releaseSCC); fill moves them into the Result.
	summ map[dataflow.L3Key][]SummaryEdge
	// modelled is the Models keys some targeted stand-in has.
	modelled map[string]bool
	done     bool
}

// onCacheMiss, when set, is called each time the builder builds unit k's
// entry of a per-unit cache ("graph", "sites" or "wired"), or recomputes
// its summary edges because releaseSCC's ("summ") are gone. A test seam:
// TestSDG_ReleasedCachesAreNotRebuilt checks that, with the memory levers
// on, no entry is built twice — a cache released before its last reader
// (releaseSCC, releaseUnit) is rebuilt silently, costing time, not facts.
var onCacheMiss func(cache string, k dataflow.L3Key)

func newSDGBuilder(in Input, d *dispatch) *sdgBuilder {
	return &sdgBuilder{
		in:       in,
		d:        d,
		p:        newPorts(in, d),
		flows:    map[Target]*flowSet{},
		info:     map[Target]*formalInfo{},
		sites:    map[dataflow.L3Key][]siteFlow{},
		graph:    map[dataflow.L3Key]*unitGraph{},
		wired:    map[dataflow.L3Key][]PortEdge{},
		summ:     map[dataflow.L3Key][]SummaryEdge{},
		modelled: map[string]bool{},
	}
}

// flowSet is a target's flows: bit (i, j) is set iff formal_in i may flow
// to formal_out j.
type flowSet struct {
	nIn, nOut, words int
	bits             []uint64
}

func newFlowSet(nIn, nOut int) *flowSet {
	w := (nOut + 63) / 64
	return &flowSet{nIn: nIn, nOut: nOut, words: w, bits: make([]uint64, nIn*w)}
}

func (f *flowSet) set(i, j int) { f.bits[i*f.words+j/64] |= 1 << (j % 64) }

func (f *flowSet) has(i, j int) bool {
	return f != nil && i < f.nIn && j < f.nOut && f.bits[i*f.words+j/64]&(1<<(j%64)) != 0
}

func (f *flowSet) count() int {
	n := 0
	if f != nil {
		for _, w := range f.bits {
			n += bits.OnesCount64(w)
		}
	}
	return n
}

// equal reports whether f and g hold the same flows (nil holds none).
func (f *flowSet) equal(g *flowSet) bool {
	if f.count() == 0 || g.count() == 0 {
		return f.count() == g.count()
	}
	return f.nIn == g.nIn && f.nOut == g.nOut && slices.Equal(f.bits, g.bits)
}

func (f *flowSet) pairs() [][2]int {
	var out [][2]int
	if f == nil {
		return nil
	}
	for i := range f.nIn {
		for j := range f.nOut {
			if f.has(i, j) {
				out = append(out, [2]int{i, j})
			}
		}
	}
	return out
}

// formalInfo is a target's formals by index: their keys' positions and
// their `of`s.
type formalInfo struct {
	inPos, outPos map[string]int
	inOf, outOf   []string
}

func (b *sdgBuilder) formalInfo(t Target) *formalInfo {
	if fi, ok := b.info[t]; ok {
		return fi
	}
	in, out := b.p.formals(t)
	fi := &formalInfo{inPos: make(map[string]int, len(in)), outPos: make(map[string]int, len(out))}
	for i, v := range in {
		fi.inPos[v.Key] = i
		fi.inOf = append(fi.inOf, v.Of)
	}
	for j, v := range out {
		fi.outPos[v.Key] = j
		fi.outOf = append(fi.outOf, v.Of)
	}
	b.info[t] = fi
	return fi
}

// siteFlow is one resolved call site of a unit: its actuals and, per
// target, which actual binds which of that target's formals (by index).
type siteFlow struct {
	in, out []Vertex
	binds   []siteBind
}

type siteBind struct {
	t         Target
	ins, outs []bindPos
}

// bindPos binds the actual at index actual (into siteFlow.in or .out) to a
// target's formal at index formal.
type bindPos struct{ actual, formal int }

// sitesOf returns the resolved call sites of unit k that have vertices (the
// sites vertices() gives actuals), in u.Calls order. Capture sites are not
// calls (spec §4.6) and are not among them.
func (b *sdgBuilder) sitesOf(k dataflow.L3Key) []siteFlow {
	if s, ok := b.sites[k]; ok {
		return s
	}
	if onCacheMiss != nil {
		onCacheMiss("sites", k)
	}
	var out []siteFlow
	for _, c := range b.p.units[k].Calls {
		s := b.p.callSite(k, c.Key, b.p.siteTargets(k, c.Key))
		if len(s.In)+len(s.Out) == 0 {
			continue
		}
		sf := siteFlow{in: s.In, out: s.Out}
		inIdx := make(map[string]int, len(s.In))
		for i, v := range s.In {
			inIdx[v.Key] = i
		}
		outIdx := make(map[string]int, len(s.Out))
		for i, v := range s.Out {
			outIdx[v.Key] = i
		}
		byT := map[Target]*siteBind{}
		var order []Target
		bind := func(t Target) *siteBind {
			sb, ok := byT[t]
			if !ok {
				sb = &siteBind{t: t}
				byT[t] = sb
				order = append(order, t)
			}
			return sb
		}
		for _, pb := range s.BindIn {
			if j, ok := b.formalInfo(pb.Target).inPos[pb.Formal]; ok {
				sb := bind(pb.Target)
				sb.ins = append(sb.ins, bindPos{inIdx[pb.Actual], j})
			}
		}
		for _, pb := range s.BindOut {
			if j, ok := b.formalInfo(pb.Target).outPos[pb.Formal]; ok {
				sb := bind(pb.Target)
				sb.outs = append(sb.outs, bindPos{outIdx[pb.Actual], j})
			}
		}
		slices.SortFunc(order, compareTarget)
		for _, t := range order {
			sf.binds = append(sf.binds, *byT[t])
		}
		out = append(out, sf)
	}
	b.sites[k] = out
	return out
}

// matrix is the site's summary under the current flows: bit i*len(out)+j is
// set iff some target has a flow from the formal actual_in i binds to the
// formal actual_out j binds.
func (b *sdgBuilder) matrix(s siteFlow) []uint64 {
	n := len(s.in) * len(s.out)
	m := make([]uint64, (n+63)/64)
	for _, sb := range s.binds {
		f := b.flows[sb.t]
		if f.count() == 0 {
			continue
		}
		for _, ip := range sb.ins {
			for _, op := range sb.outs {
				if f.has(ip.formal, op.formal) {
					bit := ip.actual*len(s.out) + op.actual
					m[bit/64] |= 1 << (bit % 64)
				}
			}
		}
	}
	return m
}

// summaryEdges is the summary edges of every call site of unit k under the
// current flows, sorted by (Src, Dst), except those from a global actual_in
// to a global actual_out (`of` `<global>:` at both ends; ruling [user,
// 2026-10-26], spec §4.9). Under the coarse host wiring (A15) a summary edge
// only shortcuts host → actual_in → … → actual_out → host, and sources and
// sinks address formal_in, so a global → global summary adds no
// reachability for an HRB-unaware walk; it is omitted to bound the output
// (on nats-server 97.7% of all summary edges were global → global). They
// return with the deferred HRB two-phase walk (spec §11). The flows keep
// every pair: only the emitted list is filtered.
func (b *sdgBuilder) summaryEdges(k dataflow.L3Key) []SummaryEdge {
	var out []SummaryEdge
	for _, s := range b.sitesOf(k) {
		m := b.matrix(s)
		for i, ai := range s.in {
			inGlobal := strings.HasPrefix(ai.Of, ofGlobal)
			for j, ao := range s.out {
				if inGlobal && strings.HasPrefix(ao.Of, ofGlobal) {
					continue
				}
				if bit := i*len(s.out) + j; m[bit/64]&(1<<(bit%64)) != 0 {
					out = append(out, SummaryEdge{Src: ai.Key, Dst: ao.Key})
				}
			}
		}
	}
	slices.SortFunc(out, compareSummaryEdge)
	return slices.Compact(out)
}

func compareSummaryEdge(a, b SummaryEdge) int {
	if c := strings.Compare(a.Src, b.Src); c != 0 {
		return c
	}
	return strings.Compare(a.Dst, b.Dst)
}

// unitGraph is an analysed unit's flow graph without its summary edges: L3
// ddg ∪ cdg ∪ the port wiring, over node indices.
type unitGraph struct {
	idx       map[string]int
	succ      [][]int32
	ins, outs []int // node of each formal_in / formal_out, by formal index
}

// summary is unit k's summary edges: the ones releaseSCC produced, else
// summaryEdges under the current flows.
func (b *sdgBuilder) summary(k dataflow.L3Key) []SummaryEdge {
	if s, ok := b.summ[k]; ok {
		return s
	}
	if onCacheMiss != nil {
		onCacheMiss("summ", k)
	}
	return b.summaryEdges(k)
}

func (b *sdgBuilder) portEdges(k dataflow.L3Key) []PortEdge {
	if e, ok := b.wired[k]; ok {
		return e
	}
	if onCacheMiss != nil {
		onCacheMiss("wired", k)
	}
	e := b.p.wire(k)
	b.wired[k] = e
	return e
}

func (b *sdgBuilder) graphOf(k dataflow.L3Key) *unitGraph {
	if g, ok := b.graph[k]; ok {
		return g
	}
	if onCacheMiss != nil {
		onCacheMiss("graph", k)
	}
	g := &unitGraph{idx: map[string]int{}}
	node := func(key string) int {
		i, ok := g.idx[key]
		if !ok {
			i = len(g.succ)
			g.idx[key] = i
			g.succ = append(g.succ, nil)
		}
		return i
	}
	edge := func(src, dst string) {
		s := node(src)
		g.succ[s] = append(g.succ[s], int32(node(dst)))
	}
	in, out := b.p.formals(Target{Callable: k})
	for _, v := range in {
		g.ins = append(g.ins, node(v.Key))
	}
	for _, v := range out {
		g.outs = append(g.outs, node(v.Key))
	}
	r := b.in.L3[k]
	for _, e := range r.Ddg {
		edge(e.Src, e.Dst)
	}
	for _, e := range r.Cdg {
		edge(e.Src, e.Dst)
	}
	for _, e := range b.portEdges(k) {
		edge(e.Src, e.Dst)
	}
	for _, s := range b.sitesOf(k) {
		for _, v := range slices.Concat(s.in, s.out) {
			node(v.Key)
		}
	}
	for i, s := range g.succ {
		slices.Sort(s)
		g.succ[i] = slices.Compact(s)
	}
	b.graph[k] = g
	return g
}

// opaque reports whether t's flows are not computed from a body: a stand-in,
// an interface method, a bodiless declaration, or a unit without the L3
// facts the port wiring needs (l3_skipped, A16).
func (b *sdgBuilder) opaque(t Target) bool {
	if t.External != "" {
		return true
	}
	u, ok := b.p.units[t.Callable]
	return !ok || !analysed(b.in, u)
}

// opaqueFlows is the flows of an opaque target: every formal_in → every
// formal_out (spec §6, §4.8 D7, A16), unless it is a stand-in with a valid
// Models entry (modelFlows). An interface method's flows are all-to-all
// whatever its VTA targets are, so the D7 union over them adds nothing.
func (b *sdgBuilder) opaqueFlows(t Target) *flowSet {
	fi := b.formalInfo(t)
	if f, ok := b.modelFlows(t, fi); ok {
		return f
	}
	f := newFlowSet(len(fi.inOf), len(fi.outOf))
	for i := range f.nIn {
		for j := range f.nOut {
			f.set(i, j)
		}
	}
	return f
}

// bodyFlows is the flows of analysed unit k under its callees' current
// flows: forward reachability from each @formal_in over the unit graph plus
// the summary edges at its call sites; (i, j) iff @formal_out j is reached
// from @formal_in i. Reachability from all formal_ins is solved at once: the
// graph's SCCs reachable from a formal_in are condensed (one source bitset
// per SCC) and propagated once, in topological order.
func (b *sdgBuilder) bodyFlows(k dataflow.L3Key) *flowSet {
	g := b.graphOf(k)
	f := newFlowSet(len(g.ins), len(g.outs))
	if len(g.ins) == 0 || len(g.outs) == 0 {
		return f
	}
	extra := map[int][]int32{}
	for _, s := range b.sitesOf(k) {
		m := b.matrix(s)
		for i, ai := range s.in {
			src := g.idx[ai.Key]
			for j, ao := range s.out {
				if bit := i*len(s.out) + j; m[bit/64]&(1<<(bit%64)) != 0 {
					extra[src] = append(extra[src], int32(g.idx[ao.Key]))
				}
			}
		}
	}
	succ := func(n int) ([]int32, []int32) { return g.succ[n], extra[n] }
	comp, order := condense(len(g.succ), g.ins, succ)
	w := (len(g.ins) + 63) / 64
	reach := make([]uint64, len(order)*w)
	for i, n := range g.ins {
		c := comp[n]
		reach[int(c)*w+i/64] |= 1 << (i % 64)
	}
	// order is sinks first: walk it backwards, so every SCC is complete
	// before it is pushed to its successors.
	for ci := len(order) - 1; ci >= 0; ci-- {
		src := reach[ci*w : (ci+1)*w]
		for _, n := range order[ci] {
			a, x := succ(n)
			for _, list := range [2][]int32{a, x} {
				for _, m := range list {
					cm := int(comp[m])
					if cm == ci {
						continue
					}
					dst := reach[cm*w : (cm+1)*w]
					for z := range dst {
						dst[z] |= src[z]
					}
				}
			}
		}
	}
	for j, n := range g.outs {
		c := comp[n]
		if c < 0 {
			continue
		}
		row := reach[int(c)*w : (int(c)+1)*w]
		for i := range g.ins {
			if row[i/64]&(1<<(i%64)) != 0 {
				f.set(i, j)
			}
		}
	}
	return f
}

// condense returns the SCCs of the graph of n nodes and succ reachable from
// roots (iterative Tarjan): comp[v] is v's SCC index (-1 when unreached),
// and order the SCCs' members, sinks first (an SCC comes after every SCC it
// reaches).
func condense(n int, roots []int, succ func(int) ([]int32, []int32)) ([]int32, [][]int) {
	comp := make([]int32, n)
	index := make([]int32, n)
	low := make([]int32, n)
	for i := range comp {
		comp[i], index[i] = -1, -1
	}
	onStack := make([]bool, n)
	var stack []int
	var order [][]int
	type frame struct{ v, next int }
	var calls []frame
	next := int32(0)
	edge := func(v, i int) (int, bool) {
		a, x := succ(v)
		if i < len(a) {
			return int(a[i]), true
		}
		if i -= len(a); i < len(x) {
			return int(x[i]), true
		}
		return 0, false
	}
	visit := func(v int) {
		index[v], low[v] = next, next
		next++
		stack = append(stack, v)
		onStack[v] = true
		calls = append(calls, frame{v: v})
	}
	for _, r := range roots {
		if index[r] >= 0 {
			continue
		}
		visit(r)
		for len(calls) > 0 {
			top := len(calls) - 1
			fr := &calls[top]
			v := fr.v
			if w, ok := edge(v, fr.next); ok {
				fr.next++
				switch {
				case index[w] < 0:
					visit(w)
				case onStack[w]:
					low[v] = min(low[v], index[w])
				}
				continue
			}
			calls = calls[:top]
			if top > 0 {
				p := calls[top-1].v
				low[p] = min(low[p], low[v])
			}
			if low[v] != index[v] {
				continue
			}
			i := len(stack) - 1
			for stack[i] != v {
				i--
			}
			members := slices.Clone(stack[i:])
			stack = stack[:i]
			c := int32(len(order))
			for _, m := range members {
				onStack[m] = false
				comp[m] = c
			}
			order = append(order, members)
		}
	}
	return comp, order
}

// solve recomputes t's flows and reports whether they changed.
func (b *sdgBuilder) solve(t Target) bool {
	var f *flowSet
	if b.opaque(t) {
		if _, ok := b.flows[t]; ok {
			return false // constant
		}
		f = b.opaqueFlows(t)
	} else {
		f = b.bodyFlows(t.Callable)
	}
	old, had := b.flows[t]
	b.flows[t] = f
	return !had && f.count() > 0 || had && !old.equal(f)
}

// summarize computes the flows of every unit and call target (spec §4.9):
// (i, j) pairs, formal_in index i → formal_out index j, sorted. Callees come
// first over the call graph's SCCs (callGraph, sccs); a recursive SCC
// iterates to its least fixpoint.
func (b *sdgBuilder) summarize() map[Target][][2]int {
	b.computeFlows()
	out := make(map[Target][][2]int, len(b.flows))
	for t, f := range b.flows {
		out[t] = f.pairs()
	}
	return out
}

func (b *sdgBuilder) computeFlows() {
	if b.done {
		return
	}
	b.done = true
	nodes, callees := callGraph(b.in, b.d)
	comps := sccs(nodes, func(t Target) []Target { return callees[t] })
	if !memLevers {
		solveSCCs(comps, callees, b.solve)
		return
	}
	for _, comp := range comps {
		solveSCCs([][]Target{comp}, callees, b.solve)
		b.releaseSCC(comp)
	}
}

// releaseSCC produces the summary edges of every unit of the solved SCC comp
// and drops its unit graphs and call sites (a memory lever, spec §4.13). A
// unit's call-site targets are all its callees (callGraph), so once its SCC
// is solved every flow its summaries read is final: no SCC solved later
// changes them, and no other unit reads its graph or sites.
func (b *sdgBuilder) releaseSCC(comp []Target) {
	for _, t := range comp {
		if t.External != "" {
			continue
		}
		k := t.Callable
		if _, unit := b.p.units[k]; !unit {
			continue
		}
		b.summ[k] = b.summaryEdges(k)
		delete(b.graph, k)
		delete(b.sites, k)
	}
}

// solveSCCs solves comps in order (callees first, as sccs returns them). A
// non-recursive singleton is solved once. A recursive SCC (several members,
// or one calling itself) is iterated to a fixpoint with a worklist: every
// member once, then each member whose callee in the SCC changed. solve
// recomputes a target's flows from its callees' current ones and reports a
// change; it must be monotone, so over finite flow sets this terminates.
func solveSCCs(comps [][]Target, callees map[Target][]Target, solve func(Target) bool) {
	for _, comp := range comps {
		if len(comp) == 1 && !slices.Contains(callees[comp[0]], comp[0]) {
			solve(comp[0])
			continue
		}
		in := make(map[Target]bool, len(comp))
		for _, m := range comp {
			in[m] = true
		}
		callers := map[Target][]Target{}
		for _, m := range comp {
			for _, c := range callees[m] {
				if in[c] {
					callers[c] = append(callers[c], m)
				}
			}
		}
		work := slices.Clone(comp)
		queued := make(map[Target]bool, len(comp))
		for _, m := range comp {
			queued[m] = true
		}
		for len(work) > 0 {
			m := work[0]
			work = work[1:]
			queued[m] = false
			if !solve(m) {
				continue
			}
			for _, c := range callers[m] {
				if !queued[c] {
					queued[c] = true
					work = append(work, c)
				}
			}
		}
	}
}
