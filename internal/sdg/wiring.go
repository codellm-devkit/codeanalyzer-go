package sdg

import (
	"cmp"
	"go/ast"
	"go/types"
	"slices"
	"strings"

	"github.com/codellm-devkit/codeanalyzer-go/internal/dataflow"
)

// ProvReachingDefs is the prov of every port-wiring ddg edge (spec §4.7,
// Python's label; L3's own edges are "ssa").
const ProvReachingDefs = "reaching-defs"

// wire returns the port wiring of unit k (spec §4.7, A4, A7, A11): the
// callable-local ddg edges, prov "reaching-defs", that connect its L4
// vertices to its body. They come from re-solving reaching definitions over
// an augmented DefUse (resolve) and from the vertices themselves:
//
//   - each L3 edge @entry → n on a base with a formal_in is mirrored as
//     @formal_in:<i> → n (same var); the L3 edge stays and is not repeated;
//   - each re-solved edge d → @exit on an output base b is emitted as
//     d → the formal_out of b (var its `of`), never into @exit;
//   - each other re-solved edge not in L3 (from the A4 uses and weak defs)
//     is emitted as is;
//   - a new edge from @entry on a base with a formal_in starts at that
//     formal_in instead (Python's formal_for remap); @entry defines a named
//     result's zero value and keeps those edges;
//   - every return node → each formal_out of an unnamed (or `_`) result
//     (var `<return>` / `<return>:<i>`); a named result reaches its
//     formal_out through the @exit rule;
//   - at each resolved call site (A11): host → each actual_in, and each
//     actual_out → host (var the actual's `of`), host = the call node's
//     Parent, else the call node (a bare, `go` or deferred call);
//   - at each capture site (A6, A7): every node defining a captured base →
//     its actual_in; its actual_out → every node using the base, and → the
//     encloser's own formal_out for the base when it is an output base (a
//     named result, a by-ref param, or a capture the encloser writes).
//
// An edge's var is the `of` of the vertex it touches, the formal_out's when
// it ends at one; a body → body edge keeps the L3 path spelling.
//
// A unit with no L3 result (l3_skipped) has no body nodes to wire: it is
// wired pass-through (wireSkipped).
//
// The edges are sorted by (Src, Dst, Var) and unique; none repeats an L3
// edge.
func (p *ports) wire(k dataflow.L3Key) []PortEdge {
	u, ok := p.units[k]
	if !ok {
		return nil
	}
	w := p.newWiring(u)
	if w == nil {
		return p.wireSkipped(u)
	}
	l3 := make(map[dataflow.DdgEdge]bool, len(w.r.Ddg))
	for _, e := range w.r.Ddg {
		l3[e] = true
	}
	out := map[PortEdge]bool{}
	// add drops an L3 edge. No rule below yields one (an edge touching a
	// vertex is never L3's, and a body → body edge is emitted only when it is
	// new), so this only guards the "no PortEdge repeats L3" contract.
	add := func(src, dst, v string) {
		if !l3[dataflow.DdgEdge{Src: src, Dst: dst, Var: v}] {
			out[PortEdge{Src: src, Dst: dst, Var: v, Prov: ProvReachingDefs}] = true
		}
	}

	for _, e := range dataflow.BuildDDG(w.r.Nodes, w.r.Cfg, w.du) {
		b := dataflow.SpellBase(e.Var)
		switch {
		case e.Dst == dataflow.ExitKey:
			fo, ok := w.exitOut[e.Var]
			if !ok {
				continue
			}
			src := e.Src
			if fi, ok := w.inOf(b); ok && src == dataflow.EntryKey {
				src = fi
			}
			add(src, fo.Key, fo.Of)
		case l3[e]:
			if fi, ok := w.inOf(b); ok && e.Src == dataflow.EntryKey {
				add(fi, e.Dst, e.Var)
			}
		default:
			src := e.Src
			if fi, ok := w.inOf(b); ok && src == dataflow.EntryKey {
				src = fi
			}
			add(src, e.Dst, e.Var)
		}
	}

	for i, fo := range w.fs.results {
		if i < len(w.resObjs) && w.resObjs[i] != nil {
			continue
		}
		of := resultOf(i, len(w.fs.results))
		for _, n := range w.r.Nodes {
			if n.Kind == dataflow.KindReturn {
				add(n.Key, fo, of)
			}
		}
	}

	for _, s := range w.sites {
		for _, v := range s.site.In {
			add(s.host, v.Key, v.Of)
		}
		for _, v := range s.site.Out {
			add(v.Key, s.host, v.Of)
		}
	}

	for _, cs := range p.capSites(u, w.taken) {
		for i, v := range cs.caps {
			of := cs.ofs[i]
			for _, n := range w.r.Nodes {
				d := w.r.DefUse[n.Key]
				if hasBase(d.Defs, v) || hasBase(d.WeakDefs, v) {
					src := n.Key
					if fi, ok := w.inByObj[v]; ok && src == dataflow.EntryKey {
						src = fi
					}
					add(src, cs.in[i], of)
				}
			}
			if cs.out[i] == "" {
				continue
			}
			for _, n := range w.r.Nodes {
				if n.Key != dataflow.ExitKey && hasBase(w.r.DefUse[n.Key].Uses, v) {
					add(cs.out[i], n.Key, of)
				}
			}
			if fo, ok := w.outByObj[v]; ok {
				add(cs.out[i], fo.Key, fo.Of)
			}
		}
	}
	return sortedPortEdges(out)
}

// resolve re-solves unit k's reaching definitions over the augmented DefUse
// (newWiring): L3's ddg plus the A4 and @exit edges. nil without an L3
// result.
func (p *ports) resolve(k dataflow.L3Key) []dataflow.DdgEdge {
	u, ok := p.units[k]
	if !ok {
		return nil
	}
	w := p.newWiring(u)
	if w == nil {
		return nil
	}
	return dataflow.BuildDDG(w.r.Nodes, w.r.Cfg, w.du)
}

// wiring is one analysed unit's port-wiring inputs.
type wiring struct {
	r        dataflow.Result
	fs       *formalSet
	du       map[string]dataflow.DefUse // du': L3's DefUse, augmented
	exitOut  map[string]Vertex          // @exit use spelling → its formal_out
	inByBase map[string]string          // base spelling → formal_in key
	inByObj  map[types.Object]string    // base object → formal_in key
	outByObj map[types.Object]Vertex    // output base object → its formal_out
	resObjs  []types.Object             // per result position: its object, nil if unnamed or `_`
	sites    []hostedSite
	taken    map[string]bool // every L1 call key and kept node key (capture-site keys)
}

type hostedSite struct {
	host string
	site site
}

func (w *wiring) inOf(base string) (string, bool) {
	k, ok := w.inByBase[base]
	return k, ok
}

// newWiring builds the augmented DefUse du' of analysed unit u (nil without
// an L3 DefUse): a copy of L3's, plus
//
//  1. (A4) at each host of a resolved, kept call site, the union over its
//     targets of their footprints' GlobalReads as uses and GlobalWrites as
//     weak defs (weak defs kill nothing, so every L3 edge survives);
//  2. at @entry, a def of every global formal_in base, so that a global
//     only a callee reads still has a def (its formal_in) at the host;
//  3. at @exit, a use of every output base: the named results, the by-ref
//     params in Footprint.Mutated (the receiver too), the captures the
//     closure writes, and the footprint's GlobalWrites.
//
// A global is the unit's own object for it when its DefUse names one, else
// a nil-base path spelled "pkgpath::Name" (the spelling of an unresolved
// package member at L3), consistent within the unit.
func (p *ports) newWiring(u dataflow.Unit) *wiring {
	r, ok := p.in.L3[u.Key]
	if !ok || r.DefUse == nil || u.Info == nil {
		return nil
	}
	t := Target{Callable: u.Key}
	fs := p.formalSet(t)
	fp := p.fps[t]
	w := &wiring{
		r:        r,
		fs:       fs,
		du:       make(map[string]dataflow.DefUse, len(r.DefUse)+1),
		exitOut:  map[string]Vertex{},
		inByBase: map[string]string{},
		inByObj:  map[types.Object]string{},
		outByObj: map[types.Object]Vertex{},
		taken:    make(map[string]bool, len(u.Calls)+len(r.Nodes)),
	}
	for key, d := range r.DefUse {
		w.du[key] = d
	}
	globals := map[string]types.Object{}
	for _, d := range r.DefUse {
		for _, list := range [][]dataflow.Path{d.Defs, d.WeakDefs, d.Uses} {
			for _, path := range list {
				if dataflow.IsPkgVar(path.Base) {
					globals[dataflow.BaseSpell(path.Base)] = path.Base
				}
			}
		}
	}
	global := func(g string) dataflow.Path { return dataflow.Path{Base: globals[g], Spell: g} }
	outKey := make(map[string]string, len(fs.out))
	outs := make(map[string]Vertex, len(fs.out))
	for _, v := range fs.out {
		outKey[v.Of] = v.Key
		outs[v.Key] = v
	}
	outVertex := func(key string) Vertex { return outs[key] }

	// formal_in by base: declared formals by name, captures, globals.
	objs := formalObjs(u.Fn, u.Info)
	byName := map[string]types.Object{}
	for _, o := range objs {
		byName[o.Name()] = o
	}
	for i, of := range fs.posOf {
		w.inByBase[of] = fs.inPos[i]
		if o, ok := byName[of]; ok {
			w.inByObj[o] = fs.inPos[i]
		}
	}
	var caps []types.Object
	if lit, ok := u.Fn.(*ast.FuncLit); ok {
		caps = dataflow.FreeVars(lit, u.Info)
	}
	for _, v := range caps {
		if k, ok := fs.inByOf[ofCapture+v.Name()]; ok {
			w.inByBase[v.Name()] = k
			w.inByObj[v] = k
		}
	}
	for _, of := range fs.globIn {
		w.inByBase[strings.TrimPrefix(of, ofGlobal)] = fs.inByOf[of]
	}

	// @exit uses: the output bases, each mapped to its formal_out.
	var exitUses []dataflow.Path
	exitUse := func(path dataflow.Path, key string) {
		exitUses = append(exitUses, path)
		w.exitOut[path.Spell] = outVertex(key)
		if path.Base != nil {
			w.outByObj[path.Base] = outVertex(key)
		}
	}
	w.resObjs = resultObjs(u.Fn, u.Info)
	for i, o := range w.resObjs {
		if o != nil && i < len(fs.results) {
			exitUse(dataflow.Path{Base: o, Spell: o.Name()}, fs.results[i])
		}
	}
	for i, of := range fs.posOf {
		if key := fs.outPos[i]; key != "" {
			if o, ok := byName[of]; ok {
				exitUse(dataflow.Path{Base: o, Spell: o.Name()}, key)
			}
		}
	}
	for _, v := range caps {
		if key, ok := fs.outByOf[ofCapture+v.Name()]; ok {
			exitUse(dataflow.Path{Base: v, Spell: v.Name()}, key)
		}
	}
	for _, g := range fp.GlobalWrites {
		if key, ok := outKey[ofGlobal+g]; ok {
			exitUse(global(g), key)
		}
	}
	d := w.du[dataflow.ExitKey]
	d.Uses = append(slices.Clip(d.Uses), exitUses...)
	w.du[dataflow.ExitKey] = d

	var entryDefs []dataflow.Path
	for _, of := range fs.globIn {
		entryDefs = append(entryDefs, global(strings.TrimPrefix(of, ofGlobal)))
	}
	d = w.du[dataflow.EntryKey]
	d.Defs = append(slices.Clip(d.Defs), entryDefs...)
	w.du[dataflow.EntryKey] = d

	// Call sites: the hosts, their A4 facts and their actuals.
	nodes := make(map[string]dataflow.Node, len(r.Nodes))
	for _, n := range r.Nodes {
		nodes[n.Key] = n
		w.taken[n.Key] = true
	}
	for _, c := range u.Calls {
		w.taken[c.Key] = true
	}
	for _, c := range u.Calls {
		n, ok := nodes[c.Key]
		if !ok {
			continue // pruned: no vertices, no wiring
		}
		host := c.Key
		if n.Parent != "" {
			host = n.Parent
		}
		if _, ok := nodes[host]; !ok {
			continue
		}
		ts := p.siteTargets(u.Key, c.Key)
		if len(ts) == 0 {
			continue
		}
		var reads, writes [][]string
		for _, t := range ts {
			f := p.fps[t]
			reads, writes = append(reads, f.GlobalReads), append(writes, f.GlobalWrites)
		}
		if rs, ws := unionSorted(reads), unionSorted(writes); len(rs)+len(ws) > 0 {
			d := w.du[host]
			uses, weak := slices.Clip(d.Uses), slices.Clip(d.WeakDefs)
			for _, g := range rs {
				uses = append(uses, global(g))
			}
			for _, g := range ws {
				weak = append(weak, global(g))
			}
			d.Uses, d.WeakDefs = uses, weak
			w.du[host] = d
		}
		if s := p.callSite(u.Key, c.Key, ts); len(s.In)+len(s.Out) > 0 {
			w.sites = append(w.sites, hostedSite{host: host, site: s})
		}
	}
	return w
}

// wireSkipped is the pass-through wiring of a unit with no L3 result
// (ruling, Task 12 review; l3_skipped is a degraded code): with no body
// nodes, every formal_in → every actual_in of its call and capture sites and
// → every formal_out, and every actual_out → every formal_out. Each edge's
// var is its destination vertex's `of`.
func (p *ports) wireSkipped(u dataflow.Unit) []PortEdge {
	fin, fout := p.formals(Target{Callable: u.Key})
	var ins, outs []Vertex
	taken := make(map[string]bool, len(u.Calls))
	for _, c := range u.Calls {
		taken[c.Key] = true
		s := p.callSite(u.Key, c.Key, p.siteTargets(u.Key, c.Key))
		ins, outs = append(ins, s.In...), append(outs, s.Out...)
	}
	for _, v := range p.captureSites(u, taken) {
		if v.Kind == KindActualIn {
			ins = append(ins, v)
		} else {
			outs = append(outs, v)
		}
	}
	out := map[PortEdge]bool{}
	for _, f := range fin {
		for _, a := range slices.Concat(ins, fout) {
			out[PortEdge{Src: f.Key, Dst: a.Key, Var: a.Of, Prov: ProvReachingDefs}] = true
		}
	}
	for _, a := range outs {
		for _, f := range fout {
			out[PortEdge{Src: a.Key, Dst: f.Key, Var: f.Of, Prov: ProvReachingDefs}] = true
		}
	}
	return sortedPortEdges(out)
}

// resultObjs returns, per result position of fn, its named result object,
// nil for an unnamed or `_` one.
func resultObjs(fn ast.Node, info *types.Info) []types.Object {
	var ft *ast.FuncType
	switch fn := fn.(type) {
	case *ast.FuncDecl:
		ft = fn.Type
	case *ast.FuncLit:
		ft = fn.Type
	}
	if ft == nil || ft.Results == nil {
		return nil
	}
	var out []types.Object
	for _, f := range ft.Results.List {
		if len(f.Names) == 0 {
			out = append(out, nil)
			continue
		}
		for _, name := range f.Names {
			var o types.Object
			if name.Name != "_" {
				o = info.Defs[name]
			}
			out = append(out, o)
		}
	}
	return out
}

func hasBase(list []dataflow.Path, v types.Object) bool {
	return slices.ContainsFunc(list, func(p dataflow.Path) bool { return p.Base == v })
}

func comparePortEdge(a, b PortEdge) int {
	return cmp.Or(strings.Compare(a.Src, b.Src), strings.Compare(a.Dst, b.Dst), strings.Compare(a.Var, b.Var), strings.Compare(a.Prov, b.Prov))
}

func sortedPortEdges(set map[PortEdge]bool) []PortEdge {
	out := make([]PortEdge, 0, len(set))
	for e := range set {
		out = append(out, e)
	}
	slices.SortFunc(out, comparePortEdge)
	return out
}
