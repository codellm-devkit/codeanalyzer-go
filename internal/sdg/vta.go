package sdg

import (
	"cmp"
	"go/types"
	"slices"

	"golang.org/x/tools/go/callgraph"
	"golang.org/x/tools/go/ssa"

	"github.com/codellm-devkit/codeanalyzer-go/internal/dataflow"
	"github.com/codellm-devkit/codeanalyzer-go/internal/schema"
	"github.com/codellm-devkit/codeanalyzer-go/internal/semantic_analysis"
)

// siteKey names one call site: its unit and its L1/L3 call key.
type siteKey struct {
	unit dataflow.L3Key
	call string
}

// l2Edge is the identity of an L2 call_graph edge: L2 dedups per (declaring
// file, source signature, target signature).
type l2Edge struct{ path, src, dst string }

// dispatch holds every call site's targets (spec §4.8): the static callee L2
// resolved ∪ the callees VTA found ∪, for an interface call, the interface
// method (D7). Targets are mapped to the L3 join's keys — a unit, an
// interface method or a bodiless declaration, or an external signature — and
// deduplicated after mapping, so a generic instance and its origin, or a
// wrapper and the method it wraps, are one target. A callee that maps to
// nothing (unmapped_ssa, already reported) is not a target.
type dispatch struct {
	sites  map[siteKey][]Target      // each sorted by compareTarget, no duplicates
	static map[siteKey]Target        // the L2 static callee of a site, when it maps to a target
	bySig  map[string]dataflow.L3Key // symbol-table callable of each signature (init excepted)
	l2     map[l2Edge]bool
	l2Ext  map[string]schema.GoExternalCallee // in.Externals, read-only
	newExt map[string]schema.GoExternalCallee // externals L2 never saw
}

// compareTarget orders targets by Callable (Path, Signature), then External.
func compareTarget(a, b Target) int {
	return cmp.Or(
		cmp.Compare(a.Callable.Path, b.Callable.Path),
		cmp.Compare(a.Callable.Signature, b.Callable.Signature),
		cmp.Compare(a.External, b.External),
	)
}

// newDispatch seeds every unit's call sites with their L2 static callee: the
// callee signature L2 backfilled into the symbol table, as an external when
// L2 tagged it with an import path, else as the symbol-table callable of that
// signature. It needs no SSA, so a run that degrades to no_ssa or no_vta
// keeps the static targets (go/types dispatch only).
func newDispatch(in Input) *dispatch {
	d := &dispatch{
		sites:  map[siteKey][]Target{},
		static: map[siteKey]Target{},
		l2:     make(map[l2Edge]bool, len(in.CallGraph)),
		l2Ext:  in.Externals,
		newExt: map[string]schema.GoExternalCallee{},
	}
	for _, e := range in.CallGraph {
		d.l2[l2Edge{e.SourcePath, e.Source, e.Target}] = true
	}

	callables := map[dataflow.L3Key]schema.GoCallable{}
	bySig := map[string]dataflow.L3Key{}
	keep := func(sig string, k dataflow.L3Key) {
		// A signature names one callable, except init (never a callee).
		if old, ok := bySig[sig]; !ok || k.Path < old.Path {
			bySig[sig] = k
		}
	}
	var walk func(c schema.GoCallable)
	walk = func(c schema.GoCallable) {
		callables[dataflow.L3Key{Path: c.Path, Signature: c.Signature}] = c
		for _, ic := range c.InnerCallables {
			walk(ic)
		}
	}
	for path, f := range in.SymbolTable {
		for _, c := range f.Functions {
			keep(c.Signature, dataflow.L3Key{Path: c.Path, Signature: c.Signature})
			walk(c)
		}
		for _, t := range f.Types {
			for _, c := range t.Methods {
				if c.Path == "" { // an interface method, keyed by its interface's file
					keep(c.Signature, dataflow.L3Key{Path: path, Signature: c.Signature})
					continue
				}
				keep(c.Signature, dataflow.L3Key{Path: c.Path, Signature: c.Signature})
				walk(c)
			}
		}
	}

	d.bySig = bySig
	for _, u := range in.Units {
		c, ok := callables[u.Key]
		if !ok || len(c.CallSites) != len(u.Calls) { // Unit.Calls is built index by index from CallSites
			continue
		}
		for i, cs := range c.CallSites {
			if cs.CalleeSignature == nil {
				continue
			}
			sig := *cs.CalleeSignature
			site := siteKey{u.Key, u.Calls[i].Key}
			t := Target{External: sig}
			if cs.CalleeImportPath == "" {
				k, ok := bySig[sig]
				if !ok {
					continue
				}
				t = Target{Callable: k}
			}
			d.add(site, t)
			d.static[site] = t
		}
	}
	return d
}

// targets returns the dispatch targets of the call site callKey of siteUnit,
// sorted by Target (Callable Path, Signature, then External): the static L2
// callee ∪ the VTA callees, and for an interface call always the interface
// method (D7). The slice is shared: callers must not modify it.
func (d *dispatch) targets(siteUnit dataflow.L3Key, callKey string) []Target {
	return d.sites[siteKey{siteUnit, callKey}]
}

func (d *dispatch) add(site siteKey, t Target) {
	ts := d.sites[site]
	i, found := slices.BinarySearchFunc(ts, t, compareTarget)
	if !found {
		d.sites[site] = slices.Insert(ts, i, t)
	}
}

// addInvokes binds every interface call of a body-joined project function to
// its interface method (D7), whether or not L2 resolved it and whether or not
// VTA finds a concrete target. It reads instructions only, so it is safe
// before VTA.
func (d *dispatch) addInvokes(j *join, funcs map[*ssa.Function]bool) {
	for fn := range funcs {
		forCalls(fn, func(call ssa.CallInstruction) {
			cc := call.Common()
			if !cc.IsInvoke() {
				return
			}
			unit, key, ok := j.callSite(call)
			if !ok {
				return
			}
			if t, ok := d.methodTarget(j, cc.Method); ok {
				d.add(siteKey{unit, key}, t)
			}
		})
	}
}

// methodTarget is the target of the interface method m: the symbol table's
// interface method for a project interface, else the external of a library
// one. A method of a universe type (error.Error) or of an interface the
// symbol table does not record (an interface literal) has no target.
func (d *dispatch) methodTarget(j *join, m *types.Func) (Target, bool) {
	if m == nil || m.Pkg() == nil {
		return Target{}, false
	}
	if j.isLibrary(m) {
		sig := semantic_analysis.CalleeSignatureOf(m)
		d.external(m, sig)
		return Target{External: sig}, true
	}
	if k, ok := j.declKey(m.Origin()); ok && j.iface[k] {
		return Target{Callable: k}, true
	}
	return Target{}, false
}

// addVTA adds VTA's callees: every edge whose site joins a call node and
// whose callee maps to a target.
func (d *dispatch) addVTA(j *join, cg *callgraph.Graph) {
	for _, node := range cg.Nodes {
		for _, e := range node.Out {
			if e.Site == nil || e.Callee == nil || e.Callee.Func == nil {
				continue
			}
			unit, key, ok := j.callSite(e.Site)
			if !ok {
				continue
			}
			if t, ok := d.calleeTarget(j, e.Callee.Func); ok {
				d.add(siteKey{unit, key}, t)
			}
		}
	}
}

// calleeTarget maps an SSA callee to its target: the unit it joins (by body
// or as a wrapper; an instance joins its origin), an interface method, a
// bodiless declaration, or a library function's external. An unmapped project
// function is no target.
func (d *dispatch) calleeTarget(j *join, fn *ssa.Function) (Target, bool) {
	if k, ok := j.fnUnit(fn); ok {
		return Target{Callable: k}, true
	}
	if k, ok := j.ifaceMethod(fn); ok {
		return Target{Callable: k}, true
	}
	if k, ok := j.bodilessDecl(fn); ok {
		return Target{Callable: k}, true
	}
	if sig, ok := j.externalOf(fn); ok {
		d.external(fn.Object().(*types.Func), sig)
		return Target{External: sig}, true
	}
	return Target{}, false
}

// external records the library callee obj (full signature sig) when L2 did
// not, with L2's own conversion, so its entry is the one L2 would have made.
func (d *dispatch) external(obj *types.Func, sig string) {
	if _, ok := d.l2Ext[sig]; ok {
		return
	}
	if _, ok := d.newExt[sig]; ok {
		return
	}
	d.newExt[sig] = semantic_analysis.ExternalCallee(obj, sig)
}

// result is the Result of the dispatch: one call_graph edge per (caller,
// target) pair that L2 lacks, prov ["vta"], weight 1 — Source the caller's
// signature and SourcePath its declaring file, as L2 has them (closures
// included), Target the callee signature (the full external signature for an
// external) and TargetPath the callee's declaring file (empty for an
// external), since a closure of an init has a per-file signature — sorted by
// (SourcePath, Source, Target, TargetPath); and the externals only VTA
// reached.
func (d *dispatch) result(deg []Degraded) Result {
	type edgeID struct{ srcPath, src, dst, dstPath string }
	seen := map[edgeID]bool{}
	var ids []edgeID
	for site, ts := range d.sites {
		for _, t := range ts {
			id := edgeID{site.unit.Path, site.unit.Signature, t.External, ""}
			if t.External == "" {
				id.dst, id.dstPath = t.Callable.Signature, t.Callable.Path
			}
			if d.l2[l2Edge{id.srcPath, id.src, id.dst}] || seen[id] {
				continue
			}
			seen[id] = true
			ids = append(ids, id)
		}
	}
	slices.SortFunc(ids, func(a, b edgeID) int {
		return cmp.Or(cmp.Compare(a.srcPath, b.srcPath), cmp.Compare(a.src, b.src),
			cmp.Compare(a.dst, b.dst), cmp.Compare(a.dstPath, b.dstPath))
	})
	res := Result{Degraded: deg}
	for _, id := range ids {
		res.CallEdges = append(res.CallEdges, schema.GoCallEdge{
			Source:     id.src,
			Target:     id.dst,
			Type:       "CALL_DEP",
			Weight:     1,
			Provenance: []string{"vta"},
			Tags:       map[string]string{},
			SourcePath: id.srcPath,
			TargetPath: id.dstPath,
		})
	}
	if len(d.newExt) > 0 {
		res.NewExternals = d.newExt
	}
	return res
}

// ApplyTo merges the L4 call-graph additions into app (spec §4.8): the VTA
// edges into app.CallGraph (semantic_analysis.MergeEdges: union prov, sum
// weight per edge identity) and the VTA-only externals into app.Externals, so
// every new edge's target resolves. Both are additions: -a 3 ⊆ -a 4.
func (r *Result) ApplyTo(app *schema.GoApplication) {
	if len(r.CallEdges) > 0 {
		app.CallGraph = semantic_analysis.MergeEdges(app.CallGraph, r.CallEdges)
	}
	if len(r.NewExternals) > 0 && app.Externals == nil {
		app.Externals = make(map[string]schema.GoExternalCallee, len(r.NewExternals))
	}
	for sig, ext := range r.NewExternals {
		if _, ok := app.Externals[sig]; !ok {
			app.Externals[sig] = ext
		}
	}
}
