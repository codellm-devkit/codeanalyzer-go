package sdg

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/tools/go/callgraph"
	"golang.org/x/tools/go/ssa"

	"github.com/codellm-devkit/codeanalyzer-go/internal/dataflow"
)

// runPorts runs L4 over testdata/dataflow and returns its vertex builder.
func runPorts(t *testing.T) (Input, *dispatch, *ports) {
	t.Helper()
	in, _, d := runDispatch(t)
	return in, d, newPorts(in, d)
}

func fin(i, of string) Vertex  { return Vertex{Key: "@formal_in:" + i, Of: of, Kind: KindFormalIn} }
func fout(i, of string) Vertex { return Vertex{Key: "@formal_out" + i, Of: of, Kind: KindFormalOut} }
func ain(call, i, of, parent string) Vertex {
	return Vertex{Key: call + "/actual_in:" + i, Of: of, Parent: parent, Kind: KindActualIn}
}
func aout(call, i, of, parent string) Vertex {
	return Vertex{Key: call + "/actual_out" + i, Of: of, Parent: parent, Kind: KindActualOut}
}

func checkVertices(t *testing.T, what string, got, want []Vertex) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s:\n got  %+v\n want %+v", what, got, want)
	}
}

func checkFormals(t *testing.T, p *ports, tg Target, wantIn, wantOut []Vertex) {
	t.Helper()
	in, out := p.formals(tg)
	checkVertices(t, fmt.Sprint("formal_in of ", tg), in, wantIn)
	checkVertices(t, fmt.Sprint("formal_out of ", tg), out, wantOut)
}

func checkActuals(t *testing.T, p *ports, d *dispatch, unit dataflow.L3Key, call string, wantIn, wantOut []Vertex) {
	t.Helper()
	in, out := p.actuals(unit, call, d.targets(unit, call))
	checkVertices(t, "actual_in of "+unit.Signature+"@"+call, in, wantIn)
	checkVertices(t, "actual_out of "+unit.Signature+"@"+call, out, wantOut)
}

// TestVertices_FormalOrderAndOf: formal_in is receiver, declared params
// (unnamed or _ as _<i>, i the declared index), captures, globals; formal_out
// is results by position (<return>:<i>, never user names), by-ref params in
// declaration order (receiver first), written captures, global writes.
func TestVertices_FormalOrderAndOf(t *testing.T) {
	_, _, p := runPorts(t)
	checkFormals(t, p, sdgTarget("multiT.Multi"),
		[]Vertex{fin("0", "s"), fin("1", "a"), fin("2", "_1")},
		[]Vertex{fout(":0", "<return>:0"), fout(":1", "<return>:1")})
	// Mutated is p, q, s: declaration order puts s before q.
	checkFormals(t, p, sdgTarget("mutate"),
		[]Vertex{fin("0", "p"), fin("1", "s"), fin("2", "n"), fin("3", "q")},
		[]Vertex{fout(":0", "p"), fout(":1", "s"), fout(":2", "q")})
	checkFormals(t, p, sdgTarget("sqlStore.setDSN"),
		[]Vertex{fin("0", "s"), fin("1", "d")},
		[]Vertex{fout("", "s")})
	// A transitively read global is a formal_in after the declared params.
	checkFormals(t, p, sdgTarget("useConfig"),
		[]Vertex{fin("0", "<global>:"+configSpell)},
		[]Vertex{fout("", "<return>")})
	checkFormals(t, p, sdgTarget("initConfig"),
		[]Vertex{fin("0", "<global>:"+configSpell)},
		[]Vertex{fout("", "<global>:"+configSpell)})
	// A closure: captures after its params; the written capture is an out.
	checkFormals(t, p, sdgTarget("CaptureSite.closure_1"),
		[]Vertex{fin("0", "<capture>:x")},
		[]Vertex{fout(":0", "<return>"), fout(":1", "<capture>:x")})
	checkFormals(t, p, sdgTarget("DirectLit.closure_1"),
		[]Vertex{fin("0", "a"), fin("1", "<capture>:y")},
		[]Vertex{fout("", "<capture>:y")})
	// An interface method (no unit) and a library interface method: the
	// unnamed receiver is _recv.
	checkFormals(t, p, sdgTarget("Store.Get"),
		[]Vertex{fin("0", "_recv"), fin("1", "k")},
		[]Vertex{fout(":0", "<return>"), fout(":1", "_recv")})
	checkFormals(t, p, Target{External: "io.Writer.Write"},
		[]Vertex{fin("0", "_recv"), fin("1", "p")},
		[]Vertex{fout(":0", "<return>:0"), fout(":1", "<return>:1"), fout(":2", "_recv"), fout(":3", "p")})
}

// TestVertices_SingleOutIsBare: a callable with exactly one formal_out, and a
// call site with exactly one actual_out, use the bare key. A named result is
// <return>, never its name.
func TestVertices_SingleOutIsBare(t *testing.T) {
	_, d, p := runPorts(t)
	checkFormals(t, p, sdgTarget("Named"), nil, []Vertex{fout("", "<return>")})
	checkActuals(t, p, d, sdgKey("useConfig"), "122:9",
		[]Vertex{ain("122:9", "0", "<global>:"+configSpell, "122:9")},
		[]Vertex{aout("122:9", "", "<return>", "122:9")})
}

// TestVertices_VariadicCollapses: excess actuals of a variadic call collapse
// onto the last formal (one actual_in), each result of f(g()) is its own
// actual, and both results of pair() collapse onto Variadic's xs.
func TestVertices_VariadicCollapses(t *testing.T) {
	_, d, p := runPorts(t)
	k := sdgKey("CallVariadic")
	// xs is a used slice: a by-ref out after the result.
	checkActuals(t, p, d, k, "195:7",
		[]Vertex{ain("195:7", "0", "xs", "195:7")},
		[]Vertex{aout("195:7", ":0", "<return>", "195:7"), aout("195:7", ":1", "xs", "195:7")})
	checkActuals(t, p, d, k, "196:7",
		[]Vertex{ain("196:7", "0", "xs", "196:7")},
		[]Vertex{aout("196:7", ":0", "<return>", "196:7"), aout("196:7", ":1", "xs", "196:7")})
	checkActuals(t, p, d, k, "197:7",
		[]Vertex{ain("197:7", "0", "a", "197:7"), ain("197:7", "1", "b", "197:7")},
		[]Vertex{aout("197:7", "", "<return>", "197:7")})
	// pair() has no params: no actual_in; two results.
	checkActuals(t, p, d, k, "196:16", nil,
		[]Vertex{aout("196:16", ":0", "<return>:0", "196:16"), aout("196:16", ":1", "<return>:1", "196:16")})
}

// TestVertices_MethodValueBindsReceiver (A8): a method call binds its
// receiver at actual_in:0; a bound method value's function-value operand
// binds it too; a method expression's first argument binds it. The actual's
// of is the static callee's formal (the interface method's _recv, not an
// implementation's name), or the first target's without one; a function
// value that may be a plain function or a bound method keeps the receiver
// slot at 0.
func TestVertices_MethodValueBindsReceiver(t *testing.T) {
	_, d, p := runPorts(t)
	recvOut := []Vertex{aout("204:9", ":0", "<return>", "204:9"), aout("204:9", ":1", "s", "204:9")}
	checkActuals(t, p, d, sdgKey("MethodValue"), "204:9",
		[]Vertex{ain("204:9", "0", "s", "204:9"), ain("204:9", "1", "k", "204:9")}, recvOut)
	checkActuals(t, p, d, sdgKey("MethodExpr"), "209:9",
		[]Vertex{ain("209:9", "0", "s", "209:9"), ain("209:9", "1", "k", "209:9")},
		[]Vertex{aout("209:9", ":0", "<return>", "209:9"), aout("209:9", ":1", "s", "209:9")})
	checkActuals(t, p, d, sdgKey("UseStore"), "34:9",
		[]Vertex{ain("34:9", "0", "_recv", "34:9"), ain("34:9", "1", "k", "34:9")},
		[]Vertex{aout("34:9", ":0", "<return>", "34:9"), aout("34:9", ":1", "_recv", "34:9")})
	// Targets bytes.Buffer.Write (sorted first) and io.Writer.Write (static).
	checkActuals(t, p, d, sdgKey("Buffered"), "93:2",
		[]Vertex{ain("93:2", "0", "_recv", "93:2"), ain("93:2", "1", "p", "93:2")},
		[]Vertex{aout("93:2", ":0", "<return>:0", "93:2"), aout("93:2", ":1", "<return>:1", "93:2"),
			aout("93:2", ":2", "_recv", "93:2"), aout("93:2", ":3", "p", "93:2")})
	// A variadic method expression passing only the receiver: v binds the
	// receiver and nothing reaches xs (no actual for it, in or out).
	checkActuals(t, p, d, sdgKey("MethodExprVariadic"), "287:9",
		[]Vertex{ain("287:9", "0", "_recv", "287:9")},
		[]Vertex{aout("287:9", "", "<return>", "287:9")})
	// sqlStore.Get (offset 1) and upper (offset 0): f binds 0 for the method.
	checkActuals(t, p, d, sdgKey("MixedValue"), "241:9",
		[]Vertex{ain("241:9", "0", "s", "241:9"), ain("241:9", "1", "k", "241:9")},
		[]Vertex{aout("241:9", ":0", "<return>", "241:9"), aout("241:9", ":1", "s", "241:9")})
}

// TestVertices_BindsEachTargetFormal: a site binds each actual to the
// formal of every target that has it, by position (receiver, params,
// results, by-ref outs) or by `of` (captures, globals) — the param_in /
// param_out endpoints. An actual a target lacks binds nothing there.
func TestVertices_BindsEachTargetFormal(t *testing.T) {
	_, d, p := runPorts(t)
	k := sdgKey("MixedValue")
	s := p.callSite(k, "241:9", d.targets(k, "241:9"))
	get, up := sdgTarget("sqlStore.Get"), sdgTarget("upper")
	wantIn := []portBind{
		{"241:9/actual_in:0", get, "@formal_in:0"},
		{"241:9/actual_in:1", get, "@formal_in:1"},
		{"241:9/actual_in:1", up, "@formal_in:0"},
	}
	if !slices.Equal(s.BindIn, wantIn) {
		t.Errorf("MixedValue BindIn = %v, want %v", s.BindIn, wantIn)
	}
	// sqlStore.Get's outs are <return> and s (by-ref); upper's only <return>.
	wantOut := []portBind{
		{"241:9/actual_out:0", get, "@formal_out:0"},
		{"241:9/actual_out:0", up, "@formal_out"},
		{"241:9/actual_out:1", get, "@formal_out:1"},
	}
	if !slices.Equal(s.BindOut, wantOut) {
		t.Errorf("MixedValue BindOut = %v, want %v", s.BindOut, wantOut)
	}
	// A transitively read global binds by `of`; a directly called literal's
	// capture binds to the literal's capture formal.
	k = sdgKey("useConfig")
	s = p.callSite(k, "122:9", d.targets(k, "122:9"))
	if want := []portBind{{"122:9/actual_in:0", sdgTarget("readConfig"), "@formal_in:0"}}; !slices.Equal(s.BindIn, want) {
		t.Errorf("useConfig BindIn = %v, want %v", s.BindIn, want)
	}
	k = sdgKey("DirectLit")
	s = p.callSite(k, "226:2", d.targets(k, "226:2"))
	lit := sdgTarget("DirectLit.closure_1")
	if want := []portBind{{"226:2/actual_in:0", lit, "@formal_in:0"}, {"226:2/actual_in:1", lit, "@formal_in:1"}}; !slices.Equal(s.BindIn, want) {
		t.Errorf("DirectLit BindIn = %v, want %v", s.BindIn, want)
	}
	if want := []portBind{{"226:2/actual_out", lit, "@formal_out"}}; !slices.Equal(s.BindOut, want) {
		t.Errorf("DirectLit BindOut = %v, want %v", s.BindOut, want)
	}
}

// TestVertices_NoVTABindsDirectLiteral: L2 resolves no FuncLit callee, so
// without VTA only the syntax names a literal called in place; its declared
// args and captures still bind at the call.
func TestVertices_NoVTABindsDirectLiteral(t *testing.T) {
	in := loadInput(t, "dataflow")
	defer func(f func(map[*ssa.Function]bool, *callgraph.Graph) *callgraph.Graph) { vtaCallGraph = f }(vtaCallGraph)
	vtaCallGraph = func(map[*ssa.Function]bool, *callgraph.Graph) *callgraph.Graph { panic("vta boom") }
	_, d := run(in)
	p := newPorts(in, d)
	k := sdgKey("DirectLit")
	if ts := d.targets(k, "226:2"); len(ts) != 0 {
		t.Fatalf("no_vta targets(DirectLit, 226:2) = %v: the test no longer covers the gap", ts)
	}
	in1, out1 := p.actuals(k, "226:2", p.siteTargets(k, "226:2"))
	checkVertices(t, "no_vta actual_in", in1, []Vertex{ain("226:2", "0", "a", "226:2"), ain("226:2", "1", "<capture>:y", "226:2")})
	checkVertices(t, "no_vta actual_out", out1, []Vertex{aout("226:2", "", "<capture>:y", "226:2")})
}

// TestVertices_DirectlyCalledLiteralBindsAtCall (A6): a literal called in
// place binds its captures at that call node after the declared args, and
// has no capture site; so does a deferred one and one whose call is a
// satellite whose statement shares its position.
func TestVertices_DirectlyCalledLiteralBindsAtCall(t *testing.T) {
	_, d, p := runPorts(t)
	checkActuals(t, p, d, sdgKey("DirectLit"), "226:2",
		[]Vertex{ain("226:2", "0", "a", "226:2"), ain("226:2", "1", "<capture>:y", "226:2")},
		[]Vertex{aout("226:2", "", "<capture>:y", "226:2")})
	checkActuals(t, p, d, sdgKey("ChainLit"), "259:2",
		[]Vertex{ain("259:2", "0", "<capture>:x", "259:2")},
		[]Vertex{aout("259:2", "", "<return>", "259:2")})
	flowKey := func(name string) dataflow.L3Key {
		return dataflow.L3Key{Path: filepath.Join("flow", "flow.go"), Signature: "example.com/dataflow/flow." + name}
	}
	// defer func() { err = wrapErr(err) }() at 557:8: the deferred call node.
	checkActuals(t, p, d, flowKey("DeferWrap"), "557:8",
		[]Vertex{ain("557:8", "0", "<capture>:err", "557:8")},
		[]Vertex{aout("557:8", "", "<capture>:err", "557:8")})
	for _, k := range []dataflow.L3Key{sdgKey("DirectLit"), sdgKey("ChainLit"), flowKey("DeferWrap")} {
		for _, v := range p.vertices(k) {
			if v.Kind == KindActualIn || v.Kind == KindActualOut {
				if _, ok := d.sites[siteKey{k, strings.SplitN(v.Key, "/actual", 2)[0]}]; !ok {
					t.Errorf("%s: %s is a capture site, want only call-node actuals", k.Signature, v.Key)
				}
			}
		}
	}
}

// TestVertices_CaptureSite (A6): a literal not called in place gets
// capture-site actuals keyed at its line:col, parented on the innermost CFG
// node holding it: the statement storing it, or the call it is passed to.
func TestVertices_CaptureSite(t *testing.T) {
	_, _, p := runPorts(t)
	got := capturesOf(p.vertices(sdgKey("CaptureSite")))
	checkVertices(t, "CaptureSite capture site", got, []Vertex{
		ain("215:7", "0", "<capture>:x", "215:2"),
		aout("215:7", "", "<capture>:x", "215:2"),
	})
	got = capturesOf(p.vertices(dataflow.L3Key{Path: filepath.Join("flow", "flow.go"), Signature: "example.com/dataflow/flow.OnceDo"}))
	checkVertices(t, "OnceDo capture site", got, []Vertex{
		ain("588:10", "0", "<capture>:cfg", "588:2"),
		aout("588:10", "", "<capture>:cfg", "588:2"),
	})
	// Passed to a satellite call: the assignment, not the call, hosts it;
	// the literal only reads v, so it has no actual_out.
	got = capturesOf(p.vertices(sdgKey("CaptureArg")))
	checkVertices(t, "CaptureArg capture site", got, []Vertex{ain("267:13", "0", "<capture>:v", "267:2")})
	// A literal after a return is dead: its host is pruned.
	if got := capturesOf(p.vertices(sdgKey("DeadLit"))); len(got) != 0 {
		t.Errorf("DeadLit capture site = %v, want none", got)
	}
}

// capturesOf keeps the vertices of vs that are not at a call key (l:c or
// l:c/n followed by /actual_*) of a call: the capture-site actuals, whose
// prefix is a literal's key, told apart by their <capture>: of.
func capturesOf(vs []Vertex) []Vertex {
	var out []Vertex
	for _, v := range vs {
		if (v.Kind == KindActualIn || v.Kind == KindActualOut) && strings.HasPrefix(v.Of, "<capture>:") && v.Parent != strings.SplitN(v.Key, "/actual", 2)[0] {
			out = append(out, v)
		}
	}
	return out
}

// TestVertices_PrunedAndUnresolvedCallsHaveNone: a call node the CFG pruned
// and a call with no target get no vertices.
func TestVertices_PrunedAndUnresolvedCallsHaveNone(t *testing.T) {
	_, d, p := runPorts(t)
	if ts := d.targets(sdgKey("PrunedCall"), "247:9"); len(ts) == 0 {
		t.Fatalf("PrunedCall@247:9 has no targets: the fixture no longer tests pruning")
	}
	for _, c := range []struct {
		k    dataflow.L3Key
		call string
	}{{sdgKey("PrunedCall"), "247:9"}, {sdgKey("callUnknown"), "252:9"}} {
		in, out := p.actuals(c.k, c.call, d.targets(c.k, c.call))
		if len(in)+len(out) != 0 {
			t.Errorf("%s@%s: actuals %v %v, want none", c.k.Signature, c.call, in, out)
		}
		for _, v := range p.vertices(c.k) {
			if strings.HasPrefix(v.Key, c.call+"/") {
				t.Errorf("%s has vertex %s", c.k.Signature, v.Key)
			}
		}
	}
}

// TestVertices_CaptureSiteKeyNeverCollides: for every callable of sdg and
// flow, vertex keys are unique, sorted, and never a body key (the kept nodes
// and every L1 call key); the key allocator skips taken keys like
// BodyNodes.add.
func TestVertices_CaptureSiteKeyNeverCollides(t *testing.T) {
	in, _, p := runPorts(t)
	n := 0
	for _, u := range in.Units {
		dir := strings.SplitN(u.Key.Path, string(filepath.Separator), 2)[0]
		if dir != "sdg" && dir != "flow" {
			continue
		}
		body := map[string]bool{}
		for _, nd := range in.L3[u.Key].Nodes {
			body[nd.Key] = true
		}
		for _, c := range u.Calls {
			body[c.Key] = true
		}
		vs := p.vertices(u.Key)
		seen := map[string]bool{}
		for i, v := range vs {
			if body[v.Key] || seen[v.Key] {
				t.Errorf("%s: vertex key %s collides", u.Key.Signature, v.Key)
			}
			seen[v.Key] = true
			if i > 0 && vs[i-1].Key >= v.Key {
				t.Errorf("%s: vertices not sorted at %s", u.Key.Signature, v.Key)
			}
			n++
		}
	}
	if n == 0 {
		t.Fatal("no vertices on sdg and flow")
	}
	// The capture site takes the next free key the way BodyNodes does.
	u := unitOf(t, in, sdgKey("CaptureSite"))
	got := p.captureSites(u, map[string]bool{"215:7": true})
	if len(got) == 0 || !strings.HasPrefix(got[0].Key, "215:7/2/") {
		t.Errorf("capture site with 215:7 taken = %v, want keyed 215:7/2", got)
	}
	taken := map[string]bool{"259:2": true, "259:2/2": true}
	if got := dataflow.NextFreeKey("259:2", taken); got != "259:2/3" || !taken[got] {
		t.Errorf("dataflow.NextFreeKey(259:2) = %s, want 259:2/3 (and taken)", got)
	}
	if got := dataflow.NextFreeKey("7:1", taken); got != "7:1" {
		t.Errorf("dataflow.NextFreeKey(7:1) = %s, want 7:1", got)
	}
}

func unitOf(t *testing.T, in Input, k dataflow.L3Key) dataflow.Unit {
	t.Helper()
	for _, u := range in.Units {
		if u.Key == k {
			return u
		}
	}
	t.Fatalf("no unit %v", k)
	return dataflow.Unit{}
}

// TestVertices_Deterministic: two builders over two loads agree.
func TestVertices_Deterministic(t *testing.T) {
	in1, _, p1 := runPorts(t)
	_, _, p2 := runPorts(t)
	for _, u := range in1.Units {
		if a, b := p1.vertices(u.Key), p2.vertices(u.Key); !slices.Equal(a, b) {
			t.Errorf("%v: vertices differ across runs:\n%v\n%v", u.Key, a, b)
		}
	}
}
