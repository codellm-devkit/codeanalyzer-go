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
	ddg, _ := solveDDG(nodes, cfg, du, nil)
	return ddg
}

// BuildDDGAlias is BuildDDG with a may-alias oracle (spec §4.10): the same
// reaching-definitions fixpoint, kills and use matching, plus one rule. A
// use u at n also links from a def d in IN(n) whose base differs from u's
// when mayAlias(d, u) holds: a write to d may change the memory a read of u
// observes. The oracle decides on the whole paths (which of their prefixes
// may share memory, and whether d writes through a reference or only
// rebinds it), so it is asymmetric: d is the write, u the read.
//
// The solver consults it only within the one-step offset window: for some
// i, j ∈ {0, 1}, d's steps after its first i and u's after its first j are
// prefix-compatible, in Covers' two-directional sense with the bases
// ignored (d.f covers u.f.g; u.* covers d.f; p.g against x.f.g at j = 1).
// An oracle accepting a pair outside the window is not asked about it.
//
// Alias never kills: a strong def through one base kills only the defs of
// its own base (BuildDDG's KILL), so a def that may be overwritten through
// an alias still reaches. The edge is d.node → n with Var = u.Spell, as for
// BuildDDG. The output is BuildDDG's plus the alias edges, deduplicated,
// sorted by (Src, Dst, Var) and non-nil; a nil mayAlias yields BuildDDG.
func BuildDDGAlias(nodes []Node, cfg []CfgEdge, du map[string]DefUse, mayAlias func(a, b Path) bool) []DdgEdge {
	ddg, alias := solveDDG(nodes, cfg, du, mayAlias)
	if len(alias) == 0 {
		return ddg
	}
	set := make(map[DdgEdge]bool, len(ddg)+len(alias))
	for _, e := range ddg {
		set[e] = true
	}
	for _, e := range alias {
		set[e.DdgEdge] = true
	}
	return sortedDdg(set)
}

// AliasEdge is an edge only BuildDDGAlias's alias rule yields. Def is the
// base spelling of the def it links from when the edge leaves @entry (which
// defines several bases; the L4 port wiring starts the edge at that base's
// formal_in), else "".
type AliasEdge struct {
	DdgEdge
	Def string
}

// BuildDDGSplit is BuildDDGAlias split in two: BuildDDG's edges, sorted by
// (Src, Dst, Var), and the edges only the alias rule yields, none of whose
// DdgEdge is in the first list, sorted by (Src, Dst, Var, Def). One fixpoint
// serves both: the L4 port wiring (package sdg) maps the first to
// reaching-defs port edges and the second to its points-to delta.
func BuildDDGSplit(nodes []Node, cfg []CfgEdge, du map[string]DefUse, mayAlias func(a, b Path) bool) (ddg []DdgEdge, alias []AliasEdge) {
	return solveDDG(nodes, cfg, du, mayAlias)
}

// solveDDG is the fixpoint and use matching of BuildDDG and BuildDDGAlias:
// the BuildDDG edges, and (mayAlias non-nil) the alias-only edges, both
// sorted, the first non-nil.
func solveDDG(nodes []Node, cfg []CfgEdge, du map[string]DefUse, mayAlias func(a, b Path) bool) (ddg []DdgEdge, alias []AliasEdge) {
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
	defBucket := make([]killBucket, len(defs))
	for i, d := range defs {
		b := bucketOf(d.path)
		defBucket[i] = b
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

	// Use matching is bucketed like the kills: interference (Covers either
	// way) needs the same bucket, so a reaching def is tested only against
	// the uses of its bucket — O(defs + matches) per node rather than
	// O(defs × uses), which the L4 re-solve's global uses at hosts need.
	edges := map[DdgEdge]bool{}
	for v, k := range g.keys {
		uses := du[k].Uses
		if len(uses) == 0 {
			continue
		}
		useBuckets := make(map[killBucket][]Path, len(uses))
		for _, u := range uses {
			b := bucketOf(u)
			useBuckets[b] = append(useBuckets[b], u)
		}
		for w, x := range in[v] {
			for x != 0 {
				b := bits.TrailingZeros64(x)
				x &^= 1 << b
				d := defs[w*64+b]
				for _, u := range useBuckets[defBucket[w*64+b]] {
					if d.path.Covers(u) || u.Covers(d.path) {
						edges[DdgEdge{Src: g.keys[d.node], Dst: k, Var: u.Spell}] = true
					}
				}
			}
		}
	}

	if mayAlias != nil {
		alias = aliasEdges(g.keys, du, in, func(i int) Path { return defs[i].path }, func(i int) string { return g.keys[defs[i].node] }, edges, mayAlias)
	}
	return sortedDdg(edges), alias
}

// aliasEdges is BuildDDGAlias's alias rule over a solved fixpoint: at each
// node with uses, each reaching def is matched against each use of a
// different base within the offset window (inWindow), and the oracle
// decides. Edges already in base are skipped. The result is sorted (nil
// when empty).
func aliasEdges(keys []string, du map[string]DefUse, in [][]uint64, pathOf func(int) Path, nodeOf func(int) string, base map[DdgEdge]bool, mayAlias func(a, b Path) bool) []AliasEdge {
	type use struct {
		path  Path
		steps []string
	}
	steps := map[int][]string{} // def index → its steps; absent: not yet split
	edges := map[AliasEdge]bool{}
	for v, k := range keys {
		var uses []use
		for _, u := range du[k].Uses {
			_, s := splitSpell(u.Spell)
			uses = append(uses, use{u, s})
		}
		if len(uses) == 0 {
			continue
		}
		for w, x := range in[v] {
			for x != 0 {
				b := bits.TrailingZeros64(x)
				x &^= 1 << b
				i := w*64 + b
				ds, ok := steps[i]
				if !ok {
					_, ds = splitSpell(pathOf(i).Spell)
					steps[i] = ds
				}
				d := pathOf(i)
				src := nodeOf(i)
				def := ""
				if src == EntryKey {
					def = SpellBase(d.Spell)
				}
				for _, u := range uses {
					if u.path.Base == d.Base || !inWindow(ds, u.steps) || !mayAlias(d, u.path) {
						continue
					}
					if e := (AliasEdge{DdgEdge{Src: src, Dst: k, Var: u.path.Spell}, def}); !base[e.DdgEdge] {
						edges[e] = true
					}
				}
			}
		}
	}
	if len(edges) == 0 {
		return nil
	}
	res := make([]AliasEdge, 0, len(edges))
	for e := range edges {
		res = append(res, e)
	}
	sort.Slice(res, func(i, j int) bool {
		a, b := res[i], res[j]
		if a.DdgEdge != b.DdgEdge {
			return ddgLess(a.DdgEdge, b.DdgEdge)
		}
		return a.Def < b.Def
	})
	return res
}

// inWindow reports whether two step lists are prefix-compatible after
// dropping at most one leading step from each (BuildDDGAlias's window).
func inWindow(ds, us []string) bool {
	for i := 0; i <= min(1, len(ds)); i++ {
		for j := 0; j <= min(1, len(us)); j++ {
			if StepsCompatible(ds[i:], us[j:]) {
				return true
			}
		}
	}
	return false
}

// StepsCompatible reports whether two step lists (the steps of two paths
// after their bases, or after a common offset) are prefix-compatible: one
// covers the other (Covers with the bases ignored). The alias oracle
// compares the steps below two may-alias prefixes with it.
func StepsCompatible(a, b []string) bool { return stepsCover(a, b) || stepsCover(b, a) }

// PathSteps returns the steps of a path spelling after its base ("x.f[*]"
// → [".f", "[*]"]); a pseudo path has none.
func PathSteps(spell string) []string {
	_, s := splitSpell(spell)
	return s
}

func ddgLess(a, b DdgEdge) bool {
	if a.Src != b.Src {
		return a.Src < b.Src
	}
	if a.Dst != b.Dst {
		return a.Dst < b.Dst
	}
	return a.Var < b.Var
}

func sortedDdg(edges map[DdgEdge]bool) []DdgEdge {
	res := make([]DdgEdge, 0, len(edges))
	for e := range edges {
		res = append(res, e)
	}
	sortDdg(res)
	return res
}

func sortDdg(res []DdgEdge) {
	sort.Slice(res, func(i, j int) bool { return ddgLess(res[i], res[j]) })
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
