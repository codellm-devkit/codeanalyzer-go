package v2emit

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"slices"
	"strings"

	"github.com/codellm-devkit/codeanalyzer-go/internal/dataflow"
	v2 "github.com/codellm-devkit/codeanalyzer-go/internal/schema/v2"
	"github.com/codellm-devkit/codeanalyzer-go/internal/sdg"
	"github.com/codellm-devkit/codeanalyzer-go/internal/utils"
)

// warnf reports an L4 fact the emitter cannot place (a test seam).
var warnf = utils.Warn

// l4Overlay merges sdg.Run's result into the emitted tree (spec §4.6, §4.7,
// §4.15). It is nil below -a 4, and then the tree is exactly the L3 one.
//
// The sdg IR holds no ids; this overlay mints every one of them:
//   - a callable's vertices land in its body under their local keys, with
//     id v2.GlobalOrdinal(callable id, key), kind, of and parent (actuals
//     only); no span and, not being call nodes, no callee key;
//   - its port edges join its ddg with prov [Prov];
//   - its summary edges are its summary;
//   - a stand-in's formals land in its external_symbols entry's body, with
//     id v2.GlobalOrdinal(external id, key) — <external-id>@formal_in:<i>;
//   - param_in / param_out endpoints and the degraded callables resolve
//     through ids, the sdg.Target → id map filled in the same walk that
//     applies the L3 overlay (externals are known up front, from their
//     import path and signature).
//
// A Ref or degraded callable that names no emitted node is an upstream bug:
// it is dropped with a warning, never emitted dangling.
type l4Overlay struct {
	res *sdg.Result
	ids map[sdg.Target]string
	// unresolved records the targets already warned about.
	unresolved map[sdg.Target]bool
}

func newL4Overlay(maxLevel int, res *sdg.Result, idx sigIndex) *l4Overlay {
	if maxLevel < 4 || res == nil {
		return nil
	}
	o := &l4Overlay{
		res:        res,
		ids:        make(map[sdg.Target]string, len(res.Callables)+len(idx.externals)),
		unresolved: map[sdg.Target]bool{},
	}
	for sig, id := range idx.externals {
		o.ids[sdg.Target{External: sig}] = id
	}
	return o
}

// apply records c's id under its target key (the callable's declaring file
// and signature, the key the L3 overlay uses) and adds its L4 overlay: the
// vertices to its body, the port edges to its ddg (re-sorted by (src, dst,
// var, prov)), and its summary.
func (o *l4Overlay) apply(c *v2.Callable, key dataflow.L3Key) {
	if o == nil {
		return
	}
	t := sdg.Target{Callable: key}
	if prev, dup := o.ids[t]; dup && prev != c.ID {
		warnf("L4 emit: %s and %s share the target key %s %s", prev, c.ID, key.Path, key.Signature)
	}
	o.ids[t] = c.ID
	r, ok := o.res.Callables[key]
	if !ok {
		return
	}
	for _, v := range r.Vertices {
		if _, clash := c.Body[v.Key]; clash {
			warnf("L4 emit: vertex %s of %s collides with a body node; dropped", v.Key, c.ID)
			continue
		}
		c.Body[v.Key] = vertexNode(c.ID, v)
	}
	if len(r.Ddg) > 0 {
		ddg := slices.Grow(c.Ddg, len(r.Ddg))
		for _, e := range r.Ddg {
			ddg = append(ddg, v2.DdgEdge{Src: e.Src, Dst: e.Dst, Var: e.Var, Prov: []string{e.Prov}})
		}
		slices.SortStableFunc(ddg, compareDdg)
		c.Ddg = ddg
	}
	if len(r.Summary) > 0 {
		c.Summary = make([]v2.SummaryEdge, len(r.Summary))
		for i, e := range r.Summary {
			c.Summary[i] = v2.SummaryEdge{Src: e.Src, Dst: e.Dst}
		}
		slices.SortFunc(c.Summary, func(a, b v2.SummaryEdge) int {
			return cmp.Or(strings.Compare(a.Src, b.Src), strings.Compare(a.Dst, b.Dst))
		})
	}
}

// vertexNode is the body node of vertex v of the callable or stand-in owner.
func vertexNode(owner string, v sdg.Vertex) v2.BodyNode {
	return v2.BodyNode{Kind: string(v.Kind), ID: v2.GlobalOrdinal(owner, v.Key), Parent: v.Parent, Of: v.Of}
}

// compareDdg orders a callable's ddg by (src, dst, var, prov). L3's own
// list is sorted by (src, dst, var) with prov ["ssa"] throughout, so the
// L3 edges keep their relative order.
func compareDdg(a, b v2.DdgEdge) int {
	return cmp.Or(
		strings.Compare(a.Src, b.Src), strings.Compare(a.Dst, b.Dst), strings.Compare(a.Var, b.Var),
		slices.Compare(a.Prov, b.Prov),
	)
}

// standIns returns the stand-in formals by full callee signature (nil
// below -a 4).
func (o *l4Overlay) standIns() map[string][]sdg.Vertex {
	if o == nil {
		return nil
	}
	return o.res.Externals
}

// late returns the fields written after the symbol table — param_in,
// param_out and degraded — which need every callable's id, so they are
// computed once the module walk has filled ids.
func (o *l4Overlay) late() lateFields {
	if o == nil {
		return lateFields{}
	}
	return lateFields{
		paramIn:  o.paramEdges("param_in", o.res.ParamIn),
		paramOut: o.paramEdges("param_out", o.res.ParamOut),
		degraded: o.degraded(),
	}
}

// mintedEdge is a param edge with its owners' ids resolved: its minted
// endpoints are GlobalOrdinal(src, e.Src.Key) and GlobalOrdinal(dst,
// e.Dst.Key), built only when the edge is written.
type mintedEdge struct {
	src, dst string
	e        *sdg.ParamEdge
}

// paramEdges returns list's edges with minted endpoints, sorted by (src,
// dst, var) over the minted ids, duplicates dropped. The sort works on the
// owner ids and local keys (compareOrdinal compares the joined ids without
// building them), so the whole list is never minted at once: on the
// streaming path each edge's ids exist only while it is written.
func (o *l4Overlay) paramEdges(list string, edges []sdg.ParamEdge) iter.Seq[v2.ParamEdge] {
	ms := make([]mintedEdge, 0, len(edges))
	for i := range edges {
		e := &edges[i]
		src, okS := o.resolve(list, e.Src.Owner)
		dst, okD := o.resolve(list, e.Dst.Owner)
		if okS && okD {
			ms = append(ms, mintedEdge{src: src, dst: dst, e: e})
		}
	}
	compare := func(a, b mintedEdge) int {
		return cmp.Or(
			compareOrdinal(a.src, a.e.Src.Key, b.src, b.e.Src.Key),
			compareOrdinal(a.dst, a.e.Dst.Key, b.dst, b.e.Dst.Key),
			strings.Compare(a.e.Var, b.e.Var),
		)
	}
	slices.SortFunc(ms, compare)
	return func(yield func(v2.ParamEdge) bool) {
		for i, m := range ms {
			if i > 0 && compare(ms[i-1], m) == 0 {
				continue
			}
			if !yield(v2.ParamEdge{
				Src: v2.GlobalOrdinal(m.src, m.e.Src.Key),
				Dst: v2.GlobalOrdinal(m.dst, m.e.Dst.Key),
				Var: m.e.Var,
			}) {
				return
			}
		}
	}
}

// resolve is t's id, warning once per target that names no emitted node.
func (o *l4Overlay) resolve(what string, t sdg.Target) (string, bool) {
	if id, ok := o.ids[t]; ok {
		return id, true
	}
	if !o.unresolved[t] {
		o.unresolved[t] = true
		warnf("L4 emit: %s names %s, which is not in the tree; its edges are dropped", what, describeTarget(t))
	}
	return "", false
}

func describeTarget(t sdg.Target) string {
	if t.External != "" {
		return "external " + t.External
	}
	return fmt.Sprintf("callable %s %s", t.Callable.Path, t.Callable.Signature)
}

// degraded mints the degradation markers: each callable as its id (sorted,
// unique), entries sorted by (code, detail); nil when nothing degraded.
func (o *l4Overlay) degraded() []v2.Degraded {
	if len(o.res.Degraded) == 0 {
		return nil
	}
	out := make([]v2.Degraded, 0, len(o.res.Degraded))
	for _, d := range o.res.Degraded {
		v := v2.Degraded{Code: d.Code, Detail: d.Detail, Packages: nonEmpty(d.Packages)}
		for _, k := range d.Callables {
			if id, ok := o.resolve("degraded "+d.Code, sdg.Target{Callable: k}); ok {
				v.Callables = append(v.Callables, id)
			}
		}
		slices.Sort(v.Callables)
		v.Callables = slices.Compact(v.Callables)
		out = append(out, v)
	}
	slices.SortStableFunc(out, func(a, b v2.Degraded) int {
		return cmp.Or(strings.Compare(a.Code, b.Code), strings.Compare(a.Detail, b.Detail))
	})
	return out
}

// compareOrdinal compares GlobalOrdinal(o1, k1) with GlobalOrdinal(o2, k2)
// byte-wise without building either.
func compareOrdinal(o1, k1, o2, k2 string) int {
	if o1 == o2 {
		return compareJoined([3]string{"", ordinalSep(k1), k1}, [3]string{"", ordinalSep(k2), k2})
	}
	if n := min(len(o1), len(o2)); o1[:n] != o2[:n] {
		return strings.Compare(o1, o2)
	}
	// One owner id is a prefix of the other (a callable and its closure).
	return compareJoined([3]string{o1, ordinalSep(k1), k1}, [3]string{o2, ordinalSep(k2), k2})
}

// ordinalSep is what v2.GlobalOrdinal puts between a callable id and k.
func ordinalSep(k string) string {
	if strings.HasPrefix(k, "@") {
		return ""
	}
	return "@"
}

// compareJoined compares the concatenations of a's and b's segments.
func compareJoined(a, b [3]string) int {
	i, j, ai, bj := 0, 0, 0, 0
	for {
		for i < len(a) && ai == len(a[i]) {
			i, ai = i+1, 0
		}
		for j < len(b) && bj == len(b[j]) {
			j, bj = j+1, 0
		}
		switch {
		case i == len(a) && j == len(b):
			return 0
		case i == len(a):
			return -1
		case j == len(b):
			return 1
		}
		if ca, cb := a[i][ai], b[j][bj]; ca != cb {
			if ca < cb {
				return -1
			}
			return 1
		}
		ai, bj = ai+1, bj+1
	}
}

// lateFields are the payload fields that follow the symbol table and need
// the whole tree's ids: the application's param_in / param_out (its last
// two fields) and the envelope's degraded (its last field).
type lateFields struct {
	paramIn, paramOut iter.Seq[v2.ParamEdge]
	degraded          []v2.Degraded
}

// writeParamList writes `,"<key>":[e,e,…]` with each edge as json.Marshal
// writes it, or nothing when seq is empty (omitempty).
func writeParamList(w io.Writer, key string, seq iter.Seq[v2.ParamEdge]) error {
	if seq == nil {
		return nil
	}
	var err error
	first := true
	for e := range seq {
		data, merr := json.Marshal(e)
		if merr != nil {
			err = merr
			break
		}
		lead := ","
		if first {
			lead = `,"` + key + `":[`
			first = false
		}
		if _, err = io.WriteString(w, lead); err != nil {
			break
		}
		if _, err = w.Write(data); err != nil {
			break
		}
	}
	if err != nil || first {
		return err
	}
	_, err = io.WriteString(w, "]")
	return err
}
