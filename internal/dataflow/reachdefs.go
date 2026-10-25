package dataflow

import (
	"go/types"
	"math/bits"
	"sort"
)

// BuildDDG computes the syntactic data-dependence graph of one callable body
// (spec §4.5) by monotone forward may-reaching-definitions over k-limited
// access paths. nodes and cfg are BuildCFG's output; du is DefUseOf of every
// node, keyed by Key (a node absent from du has an empty DefUse).
//
// A definition is a (node, path) pair from the node's Defs or WeakDefs. Per
// node, OUT = GEN ∪ (IN − KILL), IN = ∪ OUT(pred):
//
//   - GEN is every def and weak def of the node.
//   - KILL is every def d, anywhere in the body, with p.Covers(d.path) for a
//     strong def p of the node (`x = …` kills x, x.f, x[*]; `x.f = …` kills
//     x.f… but not x); for a Shallow strong def p, only the defs of p itself
//     (`s = append(s, …)` kills s, not s[*]: #28). Weak defs kill nothing.
//     Strong defs carry no wildcard (DefUseOf), so a kill never over-reaches.
//     A deferred call node only weak-defines (R11), so nothing is killed
//     through the exit chain. GEN is applied after KILL, so a node's own weak
//     def (the s[*] of `s = append(s, …)`) survives its own strong def.
//     Covers needs equal bases (and, for a nil base, equal spelling heads),
//     so p is tested only against the defs of its kill bucket (#27 item 1):
//     O(defs per base) per strong def, not O(defs in the body).
//
// A node's uses read IN — the defs reaching it before its own defs apply —
// so `y = y + 1` in a loop links to itself across the back edge, never
// within one visit. Each use u at n links from every def d in IN(n) whose
// path interferes with it, d.path.Covers(u) || u.Covers(d.path), as edge
// d.node → n with Var = u.Spell: one edge per distinct use spelling.
//
// The analysis is structural (R10): edge kinds are ignored and parallel
// edges are one. The CFG nodes are the nodes with no Parent; satellite call
// nodes carry an empty DefUse (DefUseOf) and are never endpoints (R13).
// Edges with an endpoint outside that set are ignored.
//
// Every iteration order derives from sorted keys and sorted adjacency, and
// def-sets are bitsets, so the result does not depend on the order of nodes,
// cfg or the DefUse lists. The output is deduplicated, sorted by (Src, Dst,
// Var) and non-nil. Its prov (["ssa"] at L3) is added at emission.
func BuildDDG(nodes []Node, cfg []CfgEdge, du map[string]DefUse) []DdgEdge {
	g := newPdomGraph(nodes, cfg)
	n := len(g.keys)

	// Definitions, numbered in (node key, list) order; deduplicated per node.
	type def struct {
		node int
		path Path
	}
	var defs []def
	genOf := make([][]int, n)
	for v, k := range g.keys {
		d := du[k]
		seen := map[Path]bool{}
		for _, list := range [][]Path{d.Defs, d.WeakDefs} {
			for _, p := range list {
				if !seen[p] {
					seen[p] = true
					genOf[v] = append(genOf[v], len(defs))
					defs = append(defs, def{v, p})
				}
			}
		}
	}

	// Kill index (#27 item 1): p.Covers(d) and p == d both need the same
	// Base, so a strong def is tested only against the defs of its bucket.
	// A nil-base path (a pseudo path, an unresolved identifier) is bucketed
	// by its spelling head: equal heads are necessary for Covers.
	buckets := map[killBucket][]int{}
	for i, d := range defs {
		b := bucketOf(d.path)
		buckets[b] = append(buckets[b], i)
	}

	words := (len(defs) + 63) / 64
	newSet := func() []uint64 { return make([]uint64, words) }
	gen := make([][]uint64, n)
	kill := make([][]uint64, n)
	for v, k := range g.keys {
		gen[v], kill[v] = newSet(), newSet()
		for _, i := range genOf[v] {
			gen[v][i/64] |= 1 << (i % 64)
		}
		shallow := map[Path]bool{}
		for _, p := range du[k].Shallow {
			shallow[p] = true
		}
		for _, p := range du[k].Defs {
			for _, i := range buckets[bucketOf(p)] {
				if d := defs[i]; p == d.path || !shallow[p] && p.Covers(d.path) {
					kill[v][i/64] |= 1 << (i % 64)
				}
			}
		}
	}

	in := make([][]uint64, n)
	out := make([][]uint64, n)
	for v := range in {
		in[v], out[v] = newSet(), newSet()
		copy(out[v], gen[v])
	}
	queued := make([]bool, n)
	work := make([]int, n)
	for v := range work {
		work[v], queued[v] = v, true
	}
	for len(work) > 0 {
		v := work[0]
		work = work[1:]
		queued[v] = false
		for _, p := range g.pred[v] {
			for w, x := range out[p] {
				in[v][w] |= x
			}
		}
		changed := false
		for w := range out[v] {
			x := gen[v][w] | in[v][w]&^kill[v][w]
			if x != out[v][w] {
				out[v][w], changed = x, true
			}
		}
		if changed {
			for _, s := range g.succ[v] {
				if !queued[s] {
					queued[s] = true
					work = append(work, s)
				}
			}
		}
	}

	edges := map[DdgEdge]bool{}
	for v, k := range g.keys {
		uses := du[k].Uses
		if len(uses) == 0 {
			continue
		}
		for w, x := range in[v] {
			for x != 0 {
				b := bits.TrailingZeros64(x)
				x &^= 1 << b
				d := defs[w*64+b]
				for _, u := range uses {
					if d.path.Covers(u) || u.Covers(d.path) {
						edges[DdgEdge{Src: g.keys[d.node], Dst: k, Var: u.Spell}] = true
					}
				}
			}
		}
	}

	res := make([]DdgEdge, 0, len(edges))
	for e := range edges {
		res = append(res, e)
	}
	sort.Slice(res, func(i, j int) bool {
		a, b := res[i], res[j]
		if a.Src != b.Src {
			return a.Src < b.Src
		}
		if a.Dst != b.Dst {
			return a.Dst < b.Dst
		}
		return a.Var < b.Var
	})
	return res
}

// killBucket is the kill index key of a path: its Base, or for a nil Base
// its spelling head (splitSpell's base spelling; a pseudo path is all head).
// Two paths in different buckets never cover one another.
type killBucket struct {
	base types.Object
	head string
}

func bucketOf(p Path) killBucket {
	if p.Base != nil {
		return killBucket{base: p.Base}
	}
	head, _ := splitSpell(p.Spell)
	return killBucket{head: head}
}
