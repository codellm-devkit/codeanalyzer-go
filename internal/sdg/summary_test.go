package sdg

import (
	"cmp"
	"slices"
	"strings"
	"testing"

	"golang.org/x/tools/go/callgraph"
	"golang.org/x/tools/go/ssa"

	"github.com/codellm-devkit/codeanalyzer-go/internal/dataflow"
)

func se(src, dst string) SummaryEdge { return SummaryEdge{Src: src, Dst: dst} }

func checkSummary(t *testing.T, what string, got, want []SummaryEdge) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s:\n got  %+v\n want %+v", what, got, want)
	}
}

// siteSummaryOf returns the summary edges of res's unit k whose source is an
// actual of call (or capture) key call.
func siteSummaryOf(res Result, k dataflow.L3Key, call string) []SummaryEdge {
	var out []SummaryEdge
	for _, e := range res.Callables[k].Summary {
		if strings.HasPrefix(e.Src, call+"/") {
			out = append(out, e)
		}
	}
	return out
}

func checkFlows(t *testing.T, flows map[Target][][2]int, tg Target, want [][2]int) {
	t.Helper()
	got, ok := flows[tg]
	if !ok {
		t.Errorf("no flows for %v", tg)
		return
	}
	if !slices.Equal(got, want) {
		t.Errorf("flows of %v = %v, want %v", tg, got, want)
	}
}

// TestSummary_TransitiveFlow: a(x) → b(x) → c(x) returns x, so c and b each
// have the flow x → <return>, and a's call of b has the summary edge
// actual_in:0 → actual_out.
func TestSummary_TransitiveFlow(t *testing.T) {
	_, res, _ := runDispatch(t)
	b := newSDGBuilder(runPortsInput(t))
	flows := b.summarize()
	for _, name := range []string{"a", "b", "c"} {
		checkFlows(t, flows, sdgTarget(name), [][2]int{{0, 0}})
	}
	checkSummary(t, "summary of a", res.Callables[sdgKey("a")].Summary,
		[]SummaryEdge{se("308:9/actual_in:0", "308:9/actual_out")})
	checkSummary(t, "summary of b", res.Callables[sdgKey("b")].Summary,
		[]SummaryEdge{se("312:9/actual_in:0", "312:9/actual_out")})
	if s := res.Callables[sdgKey("c")].Summary; len(s) != 0 {
		t.Errorf("c calls nothing; summary = %+v", s)
	}
}

// TestSummary_NoFlowNoEdge: ignore(x, y) returns y, so its only flow is y →
// <return>, and its call site has no edge from actual_in:0.
func TestSummary_NoFlowNoEdge(t *testing.T) {
	_, res, _ := runDispatch(t)
	flows := newSDGBuilder(runPortsInput(t)).summarize()
	checkFlows(t, flows, sdgTarget("ignore"), [][2]int{{1, 0}})
	checkSummary(t, "summary of callIgnore", res.Callables[sdgKey("callIgnore")].Summary,
		[]SummaryEdge{se("326:9/actual_in:1", "326:9/actual_out")})
}

// TestSummary_RecursiveFixpoint: dropX and dropY form one recursive SCC.
// Its fixpoint is the least one: dropX's x flows nowhere, so dropY's call
// of dropX has no edge from actual_in:0.
func TestSummary_RecursiveFixpoint(t *testing.T) {
	_, res, _ := runDispatch(t)
	flows := newSDGBuilder(runPortsInput(t)).summarize()
	checkFlows(t, flows, sdgTarget("dropX"), [][2]int{{1, 0}})
	checkFlows(t, flows, sdgTarget("dropY"), [][2]int{{0, 0}})
	checkSummary(t, "summary of dropY", res.Callables[sdgKey("dropY")].Summary,
		[]SummaryEdge{se("339:9/actual_in:1", "339:9/actual_out")})
	checkSummary(t, "summary of dropX", res.Callables[sdgKey("dropX")].Summary,
		[]SummaryEdge{se("335:9/actual_in:0", "335:9/actual_out")})
}

// TestSummary_FixpointIterates: the SCC driver iterates a recursive SCC to
// its least fixpoint. In this synthetic graph p → q → p, p's flow exists only
// once q's does and q's only through r (outside the SCC) and p, so a single
// pass in member order (p first) misses p's flow.
func TestSummary_FixpointIterates(t *testing.T) {
	p, q, r := Target{External: "p"}, Target{External: "q"}, Target{External: "r"}
	callees := map[Target][]Target{p: {q}, q: {p, r}}
	comps := sccs([]Target{p, q, r}, func(t Target) []Target { return callees[t] })
	flows := map[Target][][2]int{}
	solves := 0
	solve := func(t Target) bool {
		solves++
		var f [][2]int
		switch t {
		case r:
			f = [][2]int{{0, 0}}
		case q: // q passes r's flow on
			f = flows[r]
		case p: // p passes q's flow on
			f = flows[q]
		}
		old, had := flows[t]
		flows[t] = f
		return !had && len(f) > 0 || had && !slices.Equal(old, f)
	}
	solveSCCs(comps, callees, solve)
	for _, tg := range []Target{p, q, r} {
		checkFlows(t, flows, tg, [][2]int{{0, 0}})
	}
	if solves < 4 {
		t.Errorf("solved %d times: the recursive SCC was not iterated", solves)
	}
}

// TestSummary_NoBodyTargetsAllToAll: an interface method, a stand-in and an
// l3_skipped unit get every formal_in → every formal_out; a Models entry
// overrides a stand-in, and an empty one cuts every flow.
func TestSummary_NoBodyTargetsAllToAll(t *testing.T) {
	in, d, _ := runPorts(t)
	flows := newSDGBuilder(in, d).summarize()
	all := func(nIn, nOut int) [][2]int {
		var out [][2]int
		for i := range nIn {
			for j := range nOut {
				out = append(out, [2]int{i, j})
			}
		}
		return out
	}
	// Store.Get: formal_in _recv, k; formal_out <return>, _recv.
	checkFlows(t, flows, sdgTarget("Store.Get"), all(2, 2))
	write := Target{External: "io.Writer.Write"}
	if _, ok := flows[write]; !ok {
		t.Fatalf("no flows for %v; targets of Buffered: %v", write, d.targets(sdgKey("Buffered"), "94:2"))
	}
	checkFlows(t, flows, write, all(2, 4))

	in.Models = Models{"io/Writer.Write": {{"p", "<return>:0"}}}
	flows = newSDGBuilder(in, d).summarize()
	checkFlows(t, flows, write, [][2]int{{1, 0}})
	in.Models = Models{"io/Writer.Write": {}}
	flows = newSDGBuilder(in, d).summarize()
	checkFlows(t, flows, write, nil)

	skip := map[dataflow.L3Key]bool{sdgKey("ignore"): true}
	l3 := make(map[dataflow.L3Key]dataflow.Result, len(in.L3))
	for k, r := range in.L3 {
		if !skip[k] {
			l3[k] = r
		}
	}
	in.L3, in.Models = l3, nil
	flows = newSDGBuilder(in, d).summarize()
	checkFlows(t, flows, sdgTarget("ignore"), all(2, 1))
}

// TestParams_PerTargetWithVar: UseStore's s.Get(k) has one param_in set per
// target (the interface method and both implementations), each var that
// target's formal name, and one param_out set per target.
func TestParams_PerTargetWithVar(t *testing.T) {
	_, res, _ := runDispatch(t)
	use := sdgTarget("UseStore")
	iface, mem, sql := sdgTarget("Store.Get"), sdgTarget("memStore.Get"), sdgTarget("sqlStore.Get")
	ref := func(o Target, k string) Ref { return Ref{Owner: o, Key: k} }
	var gotIn, gotOut []ParamEdge
	for _, e := range res.ParamIn {
		if e.Src.Owner == use {
			gotIn = append(gotIn, e)
		}
	}
	for _, e := range res.ParamOut {
		if e.Dst.Owner == use {
			gotOut = append(gotOut, e)
		}
	}
	wantIn := []ParamEdge{
		{ref(use, "34:9/actual_in:0"), ref(iface, "@formal_in:0"), "_recv"},
		{ref(use, "34:9/actual_in:0"), ref(mem, "@formal_in:0"), "s"},
		{ref(use, "34:9/actual_in:0"), ref(sql, "@formal_in:0"), "s"},
		{ref(use, "34:9/actual_in:1"), ref(iface, "@formal_in:1"), "k"},
		{ref(use, "34:9/actual_in:1"), ref(mem, "@formal_in:1"), "k"},
		{ref(use, "34:9/actual_in:1"), ref(sql, "@formal_in:1"), "k"},
	}
	slices.SortFunc(wantIn, compareParamEdge)
	if !slices.Equal(gotIn, wantIn) {
		t.Errorf("param_in of UseStore:\n got  %+v\n want %+v", gotIn, wantIn)
	}
	wantOut := []ParamEdge{
		{ref(iface, "@formal_out:0"), ref(use, "34:9/actual_out:0"), "<return>"},
		{ref(iface, "@formal_out:1"), ref(use, "34:9/actual_out:1"), "_recv"},
		{ref(mem, "@formal_out:0"), ref(use, "34:9/actual_out:0"), "<return>"},
		{ref(mem, "@formal_out:1"), ref(use, "34:9/actual_out:1"), "s"},
		{ref(sql, "@formal_out:0"), ref(use, "34:9/actual_out:0"), "<return>"},
		{ref(sql, "@formal_out:1"), ref(use, "34:9/actual_out:1"), "s"},
	}
	slices.SortFunc(wantOut, compareParamEdge)
	if !slices.Equal(gotOut, wantOut) {
		t.Errorf("param_out of UseStore:\n got  %+v\n want %+v", gotOut, wantOut)
	}
	// The interface method passes everything through (D7), so the site's
	// summary is all-to-all.
	checkSummary(t, "summary of UseStore", res.Callables[sdgKey("UseStore")].Summary, []SummaryEdge{
		se("34:9/actual_in:0", "34:9/actual_out:0"), se("34:9/actual_in:0", "34:9/actual_out:1"),
		se("34:9/actual_in:1", "34:9/actual_out:0"), se("34:9/actual_in:1", "34:9/actual_out:1"),
	})
}

// TestParams_CaptureSite: a stored literal's capture site gets param_in and
// param_out to the closure's capture formals, and no summary edge.
func TestParams_CaptureSite(t *testing.T) {
	_, res, _ := runDispatch(t)
	host, lit := sdgTarget("CaptureSite"), sdgTarget("CaptureSite.closure_1")
	want := []ParamEdge{
		{Ref{host, "215:7/actual_in:0"}, Ref{lit, "@formal_in:0"}, "<capture>:x"},
	}
	var got []ParamEdge
	for _, e := range res.ParamIn {
		if e.Src.Owner == host {
			got = append(got, e)
		}
	}
	if !slices.Equal(got, want) {
		t.Errorf("param_in of CaptureSite:\n got  %+v\n want %+v", got, want)
	}
	wantOut := []ParamEdge{
		{Ref{lit, "@formal_out:1"}, Ref{host, "215:7/actual_out"}, "<capture>:x"},
	}
	got = nil
	for _, e := range res.ParamOut {
		if e.Dst.Owner == host {
			got = append(got, e)
		}
	}
	if !slices.Equal(got, wantOut) {
		t.Errorf("param_out of CaptureSite:\n got  %+v\n want %+v", got, wantOut)
	}
	if s := siteSummaryOf(res, sdgKey("CaptureSite"), "215:7"); len(s) != 0 {
		t.Errorf("capture site has summary edges %+v", s)
	}
}

// vertexSet is the vertex keys of every owner in res.
func vertexSet(res Result) map[Target]map[string]bool {
	out := map[Target]map[string]bool{}
	add := func(t Target, vs []Vertex) {
		m := map[string]bool{}
		for _, v := range vs {
			m[v.Key] = true
		}
		out[t] = m
	}
	for k, c := range res.Callables {
		add(Target{Callable: k}, c.Vertices)
	}
	for sig, vs := range res.Externals {
		add(Target{External: sig}, vs)
	}
	return out
}

// TestParams_NoDanglingEndpoints: every param_in / param_out endpoint and
// every summary edge endpoint names a vertex of its owner; the lists are
// sorted and unique.
func TestParams_NoDanglingEndpoints(t *testing.T) {
	check := func(t *testing.T, res Result) {
		t.Helper()
		if len(res.ParamIn) == 0 || len(res.ParamOut) == 0 {
			t.Fatalf("param_in %d, param_out %d: want both", len(res.ParamIn), len(res.ParamOut))
		}
		vs := vertexSet(res)
		for _, list := range [][]ParamEdge{res.ParamIn, res.ParamOut} {
			for _, e := range list {
				for _, r := range []Ref{e.Src, e.Dst} {
					if !vs[r.Owner][r.Key] {
						t.Errorf("edge %+v: %v has no vertex %s", e, r.Owner, r.Key)
					}
				}
				if e.Var == "" {
					t.Errorf("edge %+v has no var", e)
				}
			}
			if !slices.IsSortedFunc(list, compareParamEdge) {
				t.Error("param edges are not sorted")
			}
			if len(slices.CompactFunc(slices.Clone(list), func(a, b ParamEdge) bool { return a == b })) != len(list) {
				t.Error("param edges repeat")
			}
		}
		for k, c := range res.Callables {
			own := vs[Target{Callable: k}]
			for _, e := range c.Summary {
				if !own[e.Src] || !own[e.Dst] {
					t.Errorf("%v: summary edge %+v dangles", k, e)
				}
			}
			if !slices.IsSortedFunc(c.Summary, compareSummaryEdge) {
				t.Errorf("%v: summary not sorted", k)
			}
		}
	}
	t.Run("vta", func(t *testing.T) {
		_, res, _ := runDispatch(t)
		check(t, res)
	})
	t.Run("no_vta", func(t *testing.T) {
		in := loadInput(t, "dataflow")
		defer func(f func(map[*ssa.Function]bool, *callgraph.Graph) *callgraph.Graph) { vtaCallGraph = f }(vtaCallGraph)
		vtaCallGraph = func(map[*ssa.Function]bool, *callgraph.Graph) *callgraph.Graph { panic("vta boom") }
		res := Run(in)
		if !hasCode(res.Degraded, CodeNoVTA) {
			t.Fatalf("degraded = %+v, want no_vta", res.Degraded)
		}
		check(t, res)
	})
}

// TestSDG_RunFillsCallablesAndExternals: Run gives every unit its vertices,
// port edges and summary, every stand-in target its formals, and an
// interface method its formals alone.
func TestSDG_RunFillsCallablesAndExternals(t *testing.T) {
	in, res, _ := runDispatch(t)
	for _, u := range in.Units {
		if _, ok := res.Callables[u.Key]; !ok {
			t.Errorf("no CallableSDG for unit %v", u.Key)
		}
	}
	got := res.Callables[sdgKey("UseStore")]
	if len(got.Vertices) == 0 || len(got.Ddg) == 0 {
		t.Errorf("UseStore: %d vertices, %d port edges", len(got.Vertices), len(got.Ddg))
	}
	iface := res.Callables[sdgKey("Store.Get")]
	checkVertices(t, "interface method Store.Get", iface.Vertices,
		[]Vertex{fin("0", "_recv"), fin("1", "k"), fout(":0", "<return>"), fout(":1", "_recv")})
	if len(iface.Ddg)+len(iface.Summary) != 0 {
		t.Errorf("Store.Get has edges: %+v", iface)
	}
	vs, ok := res.Externals["io.Writer.Write"]
	if !ok {
		t.Fatalf("Externals lack io.Writer.Write: %v", keysOf(res.Externals))
	}
	// p ([]byte) and the receiver are by-ref outs.
	want := []Vertex{fin("0", "_recv"), fin("1", "p"), fout(":0", "<return>:0"), fout(":1", "<return>:1"), fout(":2", "_recv"), fout(":3", "p")}
	slices.SortFunc(want, func(a, b Vertex) int { return cmp.Compare(a.Key, b.Key) })
	checkVertices(t, "stand-in io.Writer.Write", vs, want)
}

func keysOf[V any](m map[string]V) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// TestSDG_Deterministic: two runs over two loads give equal L4 overlays.
func TestSDG_Deterministic(t *testing.T) {
	_, a, _ := runDispatch(t)
	_, b, _ := runDispatch(t)
	if !slices.Equal(a.ParamIn, b.ParamIn) || !slices.Equal(a.ParamOut, b.ParamOut) {
		t.Error("param edges differ across runs")
	}
	for k, c := range a.Callables {
		d := b.Callables[k]
		if !slices.Equal(c.Vertices, d.Vertices) || !slices.Equal(c.Ddg, d.Ddg) || !slices.Equal(c.Summary, d.Summary) {
			t.Errorf("%v differs across runs", k)
		}
	}
}

// runPortsInput is runPorts' input and dispatch.
func runPortsInput(t *testing.T) (Input, *dispatch) {
	t.Helper()
	in, d, _ := runPorts(t)
	return in, d
}

// TestSummary_ImplicitFlowViaCdg: implicit's x reaches no return over the
// ddg, only over the cdg (the returns depend on the if), so the flow x →
// <return> needs the cdg in the graph (spec §4.9, Python precedent).
func TestSummary_ImplicitFlowViaCdg(t *testing.T) {
	flows := newSDGBuilder(runPortsInput(t)).summarize()
	checkFlows(t, flows, sdgTarget("implicit"), [][2]int{{0, 0}})
}

// TestSummary_CondensedReachMatchesBFS: bodyFlows (one pass over the
// condensed graph) equals a plain BFS from each formal_in over the same
// edges, on every analysed fixture unit, under the final callee flows.
func TestSummary_CondensedReachMatchesBFS(t *testing.T) {
	b := newSDGBuilder(runPortsInput(t))
	b.computeFlows()
	n := 0
	for _, u := range b.in.Units {
		k := u.Key
		if b.opaque(Target{Callable: k}) {
			continue
		}
		g := b.graphOf(k)
		succ := map[int][]int{}
		for v, s := range g.succ {
			for _, m := range s {
				succ[v] = append(succ[v], int(m))
			}
		}
		for _, s := range b.sitesOf(k) {
			m := b.matrix(s)
			for i, ai := range s.in {
				for j, ao := range s.out {
					if bit := i*len(s.out) + j; m[bit/64]&(1<<(bit%64)) != 0 {
						succ[g.idx[ai.Key]] = append(succ[g.idx[ai.Key]], g.idx[ao.Key])
					}
				}
			}
		}
		var want [][2]int
		for i, src := range g.ins {
			seen := map[int]bool{src: true}
			work := []int{src}
			for len(work) > 0 {
				v := work[0]
				work = work[1:]
				for _, m := range succ[v] {
					if !seen[m] {
						seen[m] = true
						work = append(work, m)
					}
				}
			}
			for j, o := range g.outs {
				if seen[o] {
					want = append(want, [2]int{i, j})
				}
			}
		}
		slices.SortFunc(want, func(a, b [2]int) int { return cmp.Or(cmp.Compare(a[0], b[0]), cmp.Compare(a[1], b[1])) })
		if got := b.bodyFlows(k).pairs(); !slices.Equal(got, want) {
			t.Errorf("%v: bodyFlows %v, BFS %v", k, got, want)
		}
		n++
	}
	if n < 50 {
		t.Errorf("checked %d units", n)
	}
}

// TestSummary_OmitsGlobalToGlobal (ruling [user, 2026-10-26], spec §4.9): a
// call site whose callee reads and writes globals gets no summary edge from
// a global actual_in to a global actual_out, but the callee's flows still
// hold those pairs, and every other summary edge stands.
func TestSummary_OmitsGlobalToGlobal(t *testing.T) {
	in, res, _ := runDispatch(t)
	b := newSDGBuilder(runPortsInput(t))
	flows := b.summarize()

	// appendConfig(v): formal_in v, Config; formal_out <return>, Config.
	// Its flows keep Config → Config.
	checkFlows(t, flows, Target{Callable: globalsKey("appendConfig")}, [][2]int{{0, 0}, {0, 1}, {1, 0}, {1, 1}})
	checkSummary(t, "summary of useAppend", res.Callables[globalsKey("useAppend")].Summary, []SummaryEdge{
		se("100:9/actual_in:0", "100:9/actual_out:0"), se("100:9/actual_in:0", "100:9/actual_out:1"),
		se("100:9/actual_in:1", "100:9/actual_out:0"),
	})
	// Get reads and writes inst and once (formal_in inst, once; formal_out
	// <return>, inst, once): inst → inst and once → once stay in its flows.
	get := Target{Callable: globalsKey("Get")}
	fl := flows[get]
	for _, p := range [][2]int{{0, 1}, {1, 2}} {
		if !slices.Contains(fl, p) {
			t.Errorf("flows of Get = %v lack the global → global pair %v", fl, p)
		}
	}
	checkSummary(t, "summary of callsGet", res.Callables[globalsKey("callsGet")].Summary, []SummaryEdge{
		se("68:33/actual_in:0", "68:33/actual_out:0"), se("68:33/actual_in:1", "68:33/actual_out:0"),
	})
	// useConfig and readConfig only read Config: their global → <return>
	// edges are unchanged.
	checkSummary(t, "summary of useConfig", res.Callables[sdgKey("useConfig")].Summary,
		[]SummaryEdge{se("122:9/actual_in:0", "122:9/actual_out")})
	checkSummary(t, "summary of readConfig", res.Callables[sdgKey("readConfig")].Summary,
		[]SummaryEdge{se("116:10/actual_in:0", "116:10/actual_out")})

	// No unit anywhere keeps a global → global summary edge.
	for _, u := range in.Units {
		c := res.Callables[u.Key]
		of := map[string]string{}
		for _, v := range c.Vertices {
			of[v.Key] = v.Of
		}
		for _, e := range c.Summary {
			if strings.HasPrefix(of[e.Src], ofGlobal) && strings.HasPrefix(of[e.Dst], ofGlobal) {
				t.Errorf("%v: global → global summary edge %+v", u.Key, e)
			}
		}
	}
}
