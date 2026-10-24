package dataflow

import "sort"

// BuildCDG computes the control-dependence graph of one callable body (spec
// §4.4) from BuildCFG's output: the kept nodes and the final CFG.
//
// Post-dominators come from the iterative Cooper-Harvey-Kennedy algorithm on
// the reverse CFG rooted at @exit (postDominators). Control dependence
// follows Ferrante-Ottenstein-Warren: for each CFG edge a→b with b ≠
// ipdom(a), every node x on the post-dominator-tree path from b up to
// ipdom(a) (excluded) gets a→x. When b post-dominates a the walk is empty.
//
// The computation is purely structural (R10): edge kinds are ignored, and
// parallel edges between one pair (defer_resume and its exception twin, …)
// are one edge. Loops are not identified by loop_back; a back edge is just an
// edge. Self-loops are skipped: x→x is never emitted.
//
// Anchoring: every CFG node except @entry that has no controller gets
// @entry→x. That includes @exit and the deferred call nodes of the exit
// chain, which post-dominate every statement and so are never controlled.
// The one rule is "every node but @entry has a controller". (Python's
// dominance.py anchors statements only and leaves @exit out.)
// @entry→@entry is never added. When nodes holds no @entry, nothing is
// anchored.
//
// The CFG nodes are the nodes with no Parent; satellite call nodes are not
// CFG steps and get no CDG edge. Edges with an endpoint outside that set are
// ignored.
//
// Precondition (guaranteed by BuildCFG, asserted by the tests, not checked
// here): every node reaches @exit. Outside it, BuildCDG never panics: a node
// that cannot reach @exit has no post-dominator, controls nothing and is
// controlled by nothing, so its only CDG edge is the @entry anchor.
//
// The output is sorted by (Src, Dst), deduplicated and non-nil.
func BuildCDG(nodes []Node, cfg []CfgEdge) []CdgEdge {
	g := newPdomGraph(nodes, cfg)
	ipdom := g.ipdoms()

	deps := map[[2]int]bool{}
	for a, succs := range g.succ {
		if ipdom[a] < 0 {
			continue
		}
		stop := ipdom[a]
		for _, b := range succs {
			if b == a || ipdom[b] < 0 {
				continue
			}
			// ipdom(a) post-dominates b, so the walk reaches stop. The root
			// guard only protects against a malformed tree.
			for x := b; x != stop; x = ipdom[x] {
				if x != a {
					deps[[2]int{a, x}] = true
				}
				if x == g.exit {
					break
				}
			}
		}
	}

	if entry, ok := g.index[EntryKey]; ok {
		controlled := make([]bool, len(g.keys))
		for d := range deps {
			controlled[d[1]] = true
		}
		for x := range g.keys {
			if x != entry && !controlled[x] {
				deps[[2]int{entry, x}] = true
			}
		}
	}

	out := make([]CdgEdge, 0, len(deps))
	for d := range deps {
		out = append(out, CdgEdge{Src: g.keys[d[0]], Dst: g.keys[d[1]]})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Src != out[j].Src {
			return out[i].Src < out[j].Src
		}
		return out[i].Dst < out[j].Dst
	})
	return out
}

// postDominators returns the immediate post-dominator of every CFG node that
// reaches @exit, keyed by local key. @exit maps to itself (the tree root).
// A node that cannot reach @exit is absent.
func postDominators(nodes []Node, cfg []CfgEdge) map[string]string {
	g := newPdomGraph(nodes, cfg)
	out := map[string]string{}
	for x, d := range g.ipdoms() {
		if d >= 0 {
			out[g.keys[x]] = g.keys[d]
		}
	}
	return out
}

// pdomGraph is the CFG over integer vertices: keys sorted, edges deduplicated
// and kind-blind, adjacency sorted. Every iteration order derives from it,
// never from map order.
type pdomGraph struct {
	keys       []string
	index      map[string]int
	succ, pred [][]int
	exit       int // -1 when there is no @exit node
}

func newPdomGraph(nodes []Node, cfg []CfgEdge) *pdomGraph {
	index := map[string]int{}
	var keys []string
	for _, n := range nodes {
		if n.Parent != "" {
			continue
		}
		if _, dup := index[n.Key]; !dup {
			index[n.Key] = -1
			keys = append(keys, n.Key)
		}
	}
	sort.Strings(keys)
	for i, k := range keys {
		index[k] = i
	}
	g := &pdomGraph{keys: keys, index: index, succ: make([][]int, len(keys)), pred: make([][]int, len(keys)), exit: -1}
	if x, ok := index[ExitKey]; ok {
		g.exit = x
	}
	seen := map[[2]int]bool{}
	for _, e := range cfg {
		a, okA := index[e.Src]
		b, okB := index[e.Dst]
		if !okA || !okB || seen[[2]int{a, b}] {
			continue
		}
		seen[[2]int{a, b}] = true
		g.succ[a] = append(g.succ[a], b)
		g.pred[b] = append(g.pred[b], a)
	}
	for i := range keys {
		sort.Ints(g.succ[i])
		sort.Ints(g.pred[i])
	}
	return g
}

// ipdoms runs Cooper-Harvey-Kennedy ("A Simple, Fast Dominance Algorithm") on
// the reverse CFG rooted at @exit: a node's reverse-graph predecessors are its
// CFG successors. It returns ipdom per vertex, -1 for a vertex that cannot
// reach @exit (or for all of them when there is no @exit).
func (g *pdomGraph) ipdoms() []int {
	ipdom := make([]int, len(g.keys))
	for i := range ipdom {
		ipdom[i] = -1
	}
	if g.exit < 0 {
		return ipdom
	}

	// Postorder of the reverse CFG from @exit (iterative DFS over sorted
	// CFG-predecessor lists).
	po := make([]int, len(g.keys)) // postorder number; valid when ipdom set
	visited := make([]bool, len(g.keys))
	var order []int
	type frame struct{ v, i int }
	stack := []frame{{g.exit, 0}}
	visited[g.exit] = true
	for len(stack) > 0 {
		top := &stack[len(stack)-1]
		if top.i < len(g.pred[top.v]) {
			w := g.pred[top.v][top.i]
			top.i++
			if !visited[w] {
				visited[w] = true
				stack = append(stack, frame{w, 0})
			}
			continue
		}
		po[top.v] = len(order)
		order = append(order, top.v)
		stack = stack[:len(stack)-1]
	}

	intersect := func(a, b int) int {
		for a != b {
			for po[a] < po[b] {
				a = ipdom[a]
			}
			for po[b] < po[a] {
				b = ipdom[b]
			}
		}
		return a
	}

	ipdom[g.exit] = g.exit
	for changed := true; changed; {
		changed = false
		for i := len(order) - 1; i >= 0; i-- { // reverse postorder
			v := order[i]
			if v == g.exit {
				continue
			}
			nd := -1
			for _, s := range g.succ[v] {
				if s == v || ipdom[s] < 0 {
					continue
				}
				if nd < 0 {
					nd = s
				} else {
					nd = intersect(nd, s)
				}
			}
			if nd >= 0 && ipdom[v] != nd {
				ipdom[v] = nd
				changed = true
			}
		}
	}
	return ipdom
}
