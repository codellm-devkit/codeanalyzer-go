package sdg

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/codellm-devkit/codeanalyzer-go/internal/dataflow"
)

func pe(src, dst, v string) PortEdge {
	return PortEdge{Src: src, Dst: dst, Var: v, Prov: ProvReachingDefs}
}

func flowKey(name string) dataflow.L3Key {
	return dataflow.L3Key{Path: filepath.Join("flow", "flow.go"), Signature: "example.com/dataflow/flow." + name}
}

// fixtureUnits returns the units of the sdg and flow fixture packages.
func fixtureUnits(in Input) []dataflow.Unit {
	var out []dataflow.Unit
	for _, u := range in.Units {
		dir := strings.SplitN(u.Key.Path, string(filepath.Separator), 2)[0]
		if dir == "sdg" || dir == "flow" {
			out = append(out, u)
		}
	}
	return out
}

func checkHas(t *testing.T, what string, got []PortEdge, want ...PortEdge) {
	t.Helper()
	for _, e := range want {
		if !slices.Contains(got, e) {
			t.Errorf("%s lacks %+v; got %+v", what, e, got)
		}
	}
}

// reaches reports whether dst is reachable from src over edges.
func reaches(edges []PortEdge, src, dst string) bool {
	seen := map[string]bool{src: true}
	work := []string{src}
	for len(work) > 0 {
		n := work[0]
		work = work[1:]
		for _, e := range edges {
			if e.Src == n && !seen[e.Dst] {
				if e.Dst == dst {
					return true
				}
				seen[e.Dst] = true
				work = append(work, e.Dst)
			}
		}
	}
	return false
}

// TestWiring_FormalInReachesUses: each @entry → n edge on a formal's base is
// mirrored as @formal_in:<i> → n with the edge's var (the @entry edge stays
// L3's). UseStore's whole wiring is pinned: the return statement hosts the
// satellite s.Get(k), defines the result and writes through s (by-ref out).
func TestWiring_FormalInReachesUses(t *testing.T) {
	_, _, p := runPorts(t)
	got := p.wire(sdgKey("UseStore"))
	want := []PortEdge{
		pe("34:2", "34:9/actual_in:0", "_recv"),
		pe("34:2", "34:9/actual_in:1", "k"),
		pe("34:2", "@formal_out:0", "<return>"),
		pe("34:2", "@formal_out:1", "s"),
		pe("34:9/actual_out:0", "34:2", "<return>"),
		pe("34:9/actual_out:1", "34:2", "_recv"),
		pe("@formal_in:0", "34:2", "s"),
		pe("@formal_in:0", "@formal_out:1", "s"),
		pe("@formal_in:1", "34:2", "k"),
	}
	if !slices.Equal(got, want) {
		t.Errorf("wire(UseStore):\n got  %+v\n want %+v", got, want)
	}
	// A field use keeps the edge's spelling.
	checkHas(t, "wire(multiT.Multi)", p.wire(sdgKey("multiT.Multi")),
		pe("@formal_in:0", "174:2", "s.n"), pe("@formal_in:1", "174:2", "a"))
	// A closure's capture formal mirrors its @entry def of the capture.
	checkHas(t, "wire(DeferWrap.closure_2)", p.wire(flowKey("DeferWrap.closure_2")),
		pe("@formal_in:0", "557:17", "err"))
}

// TestWiring_ReturnReachesFormalOut: with unnamed results, every return node
// reaches each result's formal_out (var its `of`); a named result reaches it
// through its defs reaching @exit, and the bare return adds nothing.
func TestWiring_ReturnReachesFormalOut(t *testing.T) {
	_, _, p := runPorts(t)
	checkHas(t, "wire(multiT.Multi)", p.wire(sdgKey("multiT.Multi")),
		pe("174:2", "@formal_out:0", "<return>:0"), pe("174:2", "@formal_out:1", "<return>:1"))
	got := p.wire(sdgKey("Named"))
	checkHas(t, "wire(Named)", got, pe("179:2", "@formal_out", "<return>"))
	for _, e := range got {
		if e.Src == "180:2" {
			t.Errorf("wire(Named) has %+v: a bare return defines nothing", e)
		}
	}
	// A by-ref param's writes reach its formal_out.
	checkHas(t, "wire(mutate)", p.wire(sdgKey("mutate")),
		pe("132:2", "@formal_out:0", "p"), pe("133:2", "@formal_out:1", "s"))
}

// TestWiring_NamedResultViaDeferredClosure (A7): DeferWrap's deferred
// `func() { err = wrapErr(err) }()` hosts its own actuals: the capture's
// actual_out reaches the deferred call node, whose weak def of err reaches
// @formal_out. A stored literal writing a named result or a by-ref param
// wires its capture actual_out straight to the encloser's formal_out.
func TestWiring_NamedResultViaDeferredClosure(t *testing.T) {
	_, _, p := runPorts(t)
	got := p.wire(flowKey("DeferWrap"))
	checkHas(t, "wire(DeferWrap)", got,
		pe("557:8", "557:8/actual_in:0", "<capture>:err"),
		pe("557:8/actual_out", "557:8", "<capture>:err"),
		pe("557:8", "@formal_out", "<return>"))
	if !reaches(got, "557:8/actual_out", "@formal_out") {
		t.Errorf("wire(DeferWrap): 557:8/actual_out does not reach @formal_out: %+v", got)
	}
	checkHas(t, "wire(CaptureNamed)", p.wire(sdgKey("CaptureNamed")),
		pe("293:2", "293:7/actual_in:0", "<capture>:err"),
		pe("293:7/actual_out", "293:2", "<capture>:err"),
		pe("293:7/actual_out", "295:2", "<capture>:err"),
		pe("293:7/actual_out", "@formal_out", "<return>"),
		pe("293:2", "@formal_out", "<return>"))
	checkHas(t, "wire(CaptureParam)", p.wire(sdgKey("CaptureParam")),
		pe("@formal_in:0", "301:7/actual_in:0", "<capture>:p"),
		pe("301:7/actual_out", "301:2", "<capture>:p"),
		pe("301:7/actual_out", "@formal_out", "p"),
		pe("301:2", "@formal_out", "p"))
	// A capture site passed to a call (OnceDo): its defs and uses.
	checkHas(t, "wire(OnceDo)", p.wire(flowKey("OnceDo")),
		pe("587:2", "588:10/actual_in:0", "<capture>:cfg"),
		pe("588:10/actual_out", "589:2", "<capture>:cfg"))
}

// TestWiring_TransitiveGlobalAtHost (A4): useConfig reads Config only
// through readConfig. The host uses Config, which @formal_in(Config) defines,
// and feeds the call's actual_in for it. callsGet's callee writes inst: the
// actual_out feeds the host, whose weak def of inst reaches its formal_out.
func TestWiring_TransitiveGlobalAtHost(t *testing.T) {
	_, _, p := runPorts(t)
	got := p.wire(sdgKey("useConfig"))
	checkHas(t, "wire(useConfig)", got,
		pe("@formal_in:0", "122:2", configSpell),
		pe("122:2", "122:9/actual_in:0", "<global>:"+configSpell))
	if !reaches(got, "@formal_in:0", "122:9/actual_in:0") {
		t.Errorf("wire(useConfig): @formal_in:0 does not reach 122:9/actual_in:0")
	}

	k := globalsKey("callsGet")
	inst := "<global>:" + sdgPkg + "::inst"
	var aout, fout string
	for _, v := range p.vertices(k) {
		switch {
		case v.Kind == KindActualOut && v.Of == inst:
			aout = v.Key
		case v.Kind == KindFormalOut && v.Of == inst:
			fout = v.Key
		}
	}
	if aout == "" || fout == "" {
		t.Fatalf("callsGet has no inst actual_out / formal_out: %+v", p.vertices(k))
	}
	host := strings.SplitN(aout, "/actual_out", 2)[0]
	if n := nodeOf(p.in.L3[k], host); n.Parent != "" {
		host = n.Parent
	}
	got = p.wire(k)
	checkHas(t, "wire(callsGet)", got, pe(aout, host, inst), pe(host, fout, inst))
}

func nodeOf(r dataflow.Result, key string) dataflow.Node {
	for _, n := range r.Nodes {
		if n.Key == key {
			return n
		}
	}
	return dataflow.Node{}
}

// TestWiring_L3EdgesUntouched: the augmented re-solve keeps every L3 ddg
// edge (weak defs kill nothing), no port edge repeats an L3 edge, none ends
// at @exit, every prov is reaching-defs or (the alias delta, Task 17)
// points-to, every endpoint is a kept body node or a vertex of the callable,
// and the output is sorted and unique.
func TestWiring_L3EdgesUntouched(t *testing.T) {
	in, _, p := runPorts(t)
	n := 0
	for _, u := range in.Units {
		r, ok := in.L3[u.Key]
		if !ok {
			continue
		}
		re := p.resolve(u.Key)
		l3 := map[dataflow.DdgEdge]bool{}
		for _, e := range r.Ddg {
			l3[e] = true
			if !slices.Contains(re, e) {
				t.Errorf("%s: re-solve lost L3 edge %+v", u.Key.Signature, e)
			}
		}
		ends := map[string]bool{}
		for _, nd := range r.Nodes {
			ends[nd.Key] = true
		}
		for _, v := range p.vertices(u.Key) {
			ends[v.Key] = true
		}
		got := p.wire(u.Key)
		for i, e := range got {
			n++
			if l3[dataflow.DdgEdge{Src: e.Src, Dst: e.Dst, Var: e.Var}] {
				t.Errorf("%s: port edge %+v repeats an L3 edge", u.Key.Signature, e)
			}
			if e.Dst == dataflow.ExitKey || e.Prov != ProvReachingDefs && e.Prov != ProvPointsTo || !ends[e.Src] || !ends[e.Dst] {
				t.Errorf("%s: bad port edge %+v", u.Key.Signature, e)
			}
			if i > 0 && comparePortEdge(got[i-1], e) >= 0 {
				t.Errorf("%s: port edges not sorted/unique at %+v", u.Key.Signature, e)
			}
		}
	}
	if n == 0 {
		t.Fatal("no port edges on testdata/dataflow")
	}
}

// TestWiring_PortsCarryDependence mirrors the SDK's _ports_carry_dependence
// probe per callable: every sdg/flow callable with a formal_in has a port
// edge leaving one, unless none of its formal bases is used (no node but
// @entry uses its spelling, and no call site's targets read it, A4).
func TestWiring_PortsCarryDependence(t *testing.T) {
	in, d, p := runPorts(t)
	checked := 0
	for _, u := range fixtureUnits(in) {
		if fails, ok := portsCarry(in, d, p, u); ok {
			checked++
		} else if fails {
			t.Errorf("%s: no port edge leaves a formal_in, though a formal base is used: %+v", u.Key.Signature, p.wire(u.Key))
		}
	}
	if checked < 20 {
		t.Fatalf("only %d callables with a used formal_in: the gate covers too little", checked)
	}
}

// portsCarry checks u: ok when a port edge leaves one of its formal_ins;
// fails when none does though some formal base is used. Both false: exempt.
func portsCarry(in Input, d *dispatch, p *ports, u dataflow.Unit) (fails, ok bool) {
	fin, _ := p.formals(Target{Callable: u.Key})
	if len(fin) == 0 {
		return false, false
	}
	for _, e := range p.wire(u.Key) {
		if strings.HasPrefix(e.Src, "@formal_in:") {
			return false, true
		}
	}
	return formalUsed(in, d, p, u, fin), false
}

// formalUsed reports whether some formal_in base of u is used: a node other
// than @entry uses a path with its spelling, or (a global) a resolved call
// site's target reads it. A unit with no L3 result counts as using them all.
func formalUsed(in Input, d *dispatch, p *ports, u dataflow.Unit, fin []Vertex) bool {
	r, ok := in.L3[u.Key]
	if !ok {
		return true
	}
	bases := map[string]bool{}
	for _, v := range fin {
		b := strings.TrimPrefix(strings.TrimPrefix(v.Of, ofCapture), ofGlobal)
		bases[b] = true
	}
	for key, du := range r.DefUse {
		if key == dataflow.EntryKey {
			continue
		}
		for _, use := range du.Uses {
			if bases[dataflow.SpellBase(use.Spell)] {
				return true
			}
		}
	}
	for _, c := range u.Calls {
		if p.pruned(u.Key, c.Key) {
			continue
		}
		for _, t := range p.siteTargets(u.Key, c.Key) {
			for _, g := range p.fps[t].GlobalReads {
				if bases[g] {
					return true
				}
			}
		}
	}
	return false
}

// TestWiring_L3SkippedPassThrough (ruling, Task 12 review): a unit with no
// L3 result gets capture sites hosted by the L1 call holding the literal,
// else @entry, and pass-through wiring: every formal_in feeds every actual_in,
// every actual_out every formal_out, and every formal_in every formal_out.
func TestWiring_L3SkippedPassThrough(t *testing.T) {
	in, d, _ := runPorts(t)
	skip := map[dataflow.L3Key]bool{sdgKey("CaptureSite"): true, sdgKey("CaptureArg"): true, flowKey("OnceDo"): true}
	l3 := make(map[dataflow.L3Key]dataflow.Result, len(in.L3))
	for k, r := range in.L3 {
		if !skip[k] {
			l3[k] = r
		}
	}
	in.L3 = l3
	p := newPorts(in, d)

	checkVertices(t, "skipped CaptureSite capture site", capturesOf(p.vertices(sdgKey("CaptureSite"))), []Vertex{
		ain("215:7", "0", "<capture>:x", "@entry"),
		aout("215:7", "", "<capture>:x", "@entry"),
	})
	checkVertices(t, "skipped CaptureArg capture site", capturesOf(p.vertices(sdgKey("CaptureArg"))), []Vertex{
		ain("267:13", "0", "<capture>:v", "267:7"),
	})
	checkVertices(t, "skipped OnceDo capture site", capturesOf(p.vertices(flowKey("OnceDo"))), []Vertex{
		ain("588:10", "0", "<capture>:cfg", "588:2"),
		aout("588:10", "", "<capture>:cfg", "588:2"),
	})

	got := p.wire(sdgKey("CaptureSite"))
	want := []PortEdge{
		pe("215:7/actual_out", "@formal_out", "<return>"),
		pe("@formal_in:0", "215:7/actual_in:0", "<capture>:x"),
		pe("@formal_in:0", "@formal_out", "<return>"),
	}
	if !slices.Equal(got, want) {
		t.Errorf("skipped wire(CaptureSite):\n got  %+v\n want %+v", got, want)
	}
	checkHas(t, "skipped wire(CaptureArg)", p.wire(sdgKey("CaptureArg")),
		pe("@formal_in:0", "267:13/actual_in:0", "<capture>:v"),
		pe("@formal_in:0", "267:7/actual_in:0", "f"),
		pe("267:7/actual_out:0", "@formal_out", "<return>"),
		pe("267:7/actual_out:1", "@formal_out", "<return>"))
}

// TestWiring_Deterministic: two builders over two loads agree.
func TestWiring_Deterministic(t *testing.T) {
	in1, _, p1 := runPorts(t)
	_, _, p2 := runPorts(t)
	for _, u := range in1.Units {
		if a, b := p1.wire(u.Key), p2.wire(u.Key); !slices.Equal(a, b) {
			t.Errorf("%v: port edges differ across runs:\n%v\n%v", u.Key, a, b)
		}
	}
}
