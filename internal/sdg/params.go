package sdg

import (
	"cmp"
	"slices"
	"strings"

	"github.com/codellm-devkit/codeanalyzer-go/internal/dataflow"
)

// params returns the param_in and param_out edges of unit k (spec §4.7),
// one set per target:
//
//   - at each resolved call site with vertices: param_in from each actual_in
//     to each target's formal_in it binds, and param_out from each target's
//     formal_out to the actual_out it binds;
//   - at each capture site (A6): param_in from each capture's actual_in to
//     the closure's `<capture>:` formal_in, and param_out from the closure's
//     `<capture>:` formal_out to the site's actual_out.
//
// Var is the target formal's `of`. An actual whose `of` a target lacks (a
// global another target touches) binds nothing in it, so it gets no edge.
func (b *sdgBuilder) params(k dataflow.L3Key) (in, out []ParamEdge) {
	u, ok := b.p.units[k]
	if !ok {
		return nil, nil
	}
	caller := Target{Callable: k}
	taken := make(map[string]bool, len(u.Calls))
	for _, c := range u.Calls {
		taken[c.Key] = true
		s := b.p.callSite(k, c.Key, b.p.siteTargets(k, c.Key))
		for _, pb := range s.BindIn {
			fi := b.formalInfo(pb.Target)
			if j, ok := fi.inPos[pb.Formal]; ok {
				in = append(in, ParamEdge{Src: Ref{caller, pb.Actual}, Dst: Ref{pb.Target, pb.Formal}, Var: fi.inOf[j]})
			}
		}
		for _, pb := range s.BindOut {
			fi := b.formalInfo(pb.Target)
			if j, ok := fi.outPos[pb.Formal]; ok {
				out = append(out, ParamEdge{Src: Ref{pb.Target, pb.Formal}, Dst: Ref{caller, pb.Actual}, Var: fi.outOf[j]})
			}
		}
	}
	for key := range b.p.keptKeys(k) {
		taken[key] = true
	}
	for _, cs := range b.p.capSites(u, taken) {
		lk, ok := b.p.byFn[cs.lit]
		if !ok {
			continue
		}
		lit := Target{Callable: lk}
		fs := b.p.formalSet(lit)
		for i, of := range cs.ofs {
			if f, ok := fs.inByOf[of]; ok {
				in = append(in, ParamEdge{Src: Ref{caller, cs.in[i]}, Dst: Ref{lit, f}, Var: of})
			}
			if cs.out[i] == "" {
				continue
			}
			if f, ok := fs.outByOf[of]; ok {
				out = append(out, ParamEdge{Src: Ref{lit, f}, Dst: Ref{caller, cs.out[i]}, Var: of})
			}
		}
	}
	return in, out
}

// compareParamEdge orders param edges by (Src owner, Src key, Dst owner,
// Dst key, Var), owners by compareTarget: stable before ids are minted.
func compareParamEdge(a, b ParamEdge) int {
	return cmp.Or(
		compareTarget(a.Src.Owner, b.Src.Owner), strings.Compare(a.Src.Key, b.Src.Key),
		compareTarget(a.Dst.Owner, b.Dst.Owner), strings.Compare(a.Dst.Key, b.Dst.Key),
		strings.Compare(a.Var, b.Var),
	)
}

// releaseUnit drops what fill cached for unit k once its overlay is in the
// Result: the wiring (the Result shares its slice), its summary edges and
// call sites, and the ports' call and kept-node indexes of k (read only by
// k's own vertices, wiring and params).
func (b *sdgBuilder) releaseUnit(k dataflow.L3Key) {
	delete(b.wired, k)
	delete(b.summ, k)
	delete(b.sites, k)
	delete(b.graph, k)
	delete(b.p.calls, k)
	delete(b.p.kept, k)
}

func sortParamEdges(es []ParamEdge) []ParamEdge {
	slices.SortFunc(es, compareParamEdge)
	return slices.Compact(es)
}

// fill computes the flows and fills res's L4 overlay, serially:
//
//   - Callables[k] for every unit: its vertices, port edges and call-site
//     summary edges;
//   - ParamIn / ParamOut over every unit's call and capture sites, sorted
//     by compareParamEdge;
//   - Externals[sig]: the formal vertices (sorted by key) of every stand-in
//     some call site targets (spec §6), an empty list for one with no
//     formals (runtime.Gosched);
//   - Callables[k] (vertices only) for every interface method or bodiless
//     declaration some param edge reaches (spec §4.8), so no param edge
//     dangles.
func (b *sdgBuilder) fill(res *Result) {
	b.computeFlows()
	res.Callables = make(map[dataflow.L3Key]CallableSDG, len(b.in.Units))
	var ins, outs []ParamEdge
	for _, u := range b.in.Units {
		k := u.Key
		res.Callables[k] = CallableSDG{
			Vertices: b.p.vertices(k),
			Ddg:      b.portEdges(k),
			Summary:  b.summary(k),
		}
		pi, po := b.params(k)
		ins, outs = append(ins, pi...), append(outs, po...)
		if memLevers {
			// The Result now holds unit k's overlay; nothing below reads
			// its caches again.
			b.releaseUnit(k)
		}
	}
	res.ParamIn, res.ParamOut = sortParamEdges(ins), sortParamEdges(outs)

	formals := func(t Target) []Vertex {
		in, out := b.p.formals(t)
		vs := slices.Concat(in, out)
		if vs == nil {
			vs = []Vertex{}
		}
		slices.SortFunc(vs, func(a, b Vertex) int { return strings.Compare(a.Key, b.Key) })
		return vs
	}
	for _, ts := range b.d.sites {
		for _, t := range ts {
			if t.External == "" {
				continue
			}
			if res.Externals == nil {
				res.Externals = map[string][]Vertex{}
			}
			if _, done := res.Externals[t.External]; !done {
				res.Externals[t.External] = formals(t)
			}
		}
	}

	// The in-project targets with no unit some param edge reaches.
	for _, list := range [][]ParamEdge{res.ParamIn, res.ParamOut} {
		for _, e := range list {
			for _, t := range []Target{e.Src.Owner, e.Dst.Owner} {
				if _, unit := b.p.units[t.Callable]; unit || t.External != "" {
					continue
				}
				if _, done := res.Callables[t.Callable]; !done {
					res.Callables[t.Callable] = CallableSDG{Vertices: formals(t)}
				}
			}
		}
	}
	b.unusedModels()
}
