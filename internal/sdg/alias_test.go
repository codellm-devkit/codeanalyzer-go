package sdg

import (
	"flag"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"golang.org/x/tools/go/callgraph"
	"golang.org/x/tools/go/ssa"

	"github.com/codellm-devkit/codeanalyzer-go/internal/dataflow"
)

var update = flag.Bool("update", false, "rewrite golden files")

func pt(src, dst, v string) PortEdge {
	return PortEdge{Src: src, Dst: dst, Var: v, Prov: ProvPointsTo}
}

func pointsTo(es []PortEdge) []PortEdge {
	var out []PortEdge
	for _, e := range es {
		if e.Prov == ProvPointsTo {
			out = append(out, e)
		}
	}
	return out
}

// runAlias runs L4 over testdata/dataflow with the alias pass on or off.
func runAlias(t *testing.T, on bool) Result {
	t.Helper()
	defer func(v func(dataflow.Unit, ast.Node) AliasOracle) { newAliasOracle = v }(newAliasOracle)
	if !on {
		newAliasOracle = nil
	}
	return Run(loadInput(t, "dataflow"))
}

// TestAlias_PointerCopyLinks: in AliasPtr (p := req; p.UserID = id; return
// req.UserID) the write through the copy p reaches the read through req only
// by may-alias, as a points-to edge 360:2 → 361:2 on req.UserID; the same
// write reaches req's by-ref formal_out (var its `of`, as wire maps an
// @exit edge). Those are AliasPtr's only points-to edges.
//
// AliasPtrWhole returns the bare req (ruling, Task 17: the use side is
// unrestricted): the write p.UserID is prefix-compatible with the whole read.
// The fix-round-1 fixtures (address-taken values, reference fields copied
// out, unrelated interfaces) are pinned whole too.
func TestAlias_PointerCopyLinks(t *testing.T) {
	res := runAlias(t, true)
	for _, c := range []struct {
		fn   string
		want []PortEdge
	}{
		{"AliasPtr", []PortEdge{pt("360:2", "361:2", "req.UserID"), pt("360:2", "@formal_out:1", "req")}},
		{"AliasPtrWhole", []PortEdge{pt("368:2", "369:2", "req"), pt("368:2", "@formal_out:1", "req")}},
		// aliasAssign (q := p; q.n = v): q.n = v reaches p's formal_out.
		{"aliasAssign", []PortEdge{pt("153:2", "@formal_out", "p")}},
		// Fix round 1, I3: address-taken values. The value's own def
		// (`var req Req`, storage) reaches the use p of `p.UserID = id`: p
		// points into it.
		{"AliasPtrValue", []PortEdge{pt("388:2", "390:2", "p"), pt("390:2", "391:2", "req.UserID")}},
		{"AliasArraySlice", []PortEdge{pt("396:2", "398:2", "s"), pt("398:2", "399:2", "arr[*]")}},
		{"AliasFieldAddr", []PortEdge{pt("405:2", "407:2", "p"), pt("407:2", "408:2", "o.Inner.n")}},
		// Option A: reference-typed fields copied out (one-step offset).
		{"AliasRefField", []PortEdge{pt("424:2", "425:2", "s.m[*]"), pt("424:2", "@formal_out:1", "s")}},
		{"AliasPtrField", []PortEdge{pt("436:2", "437:2", "r.ptr.n"), pt("436:2", "@formal_out:1", "r")}},
		// I2: an io.Writer and an io.Reader holding one *bytes.Buffer.
		{"AliasIfaces", []PortEdge{pt("445:2", "446:2", "r"), pt("445:2", "@formal_out:1", "f"), pt("446:2", "@formal_out:1", "f")}},
		// Fix round 2, N1: alias edges from @entry whose def is another base
		// than the use's start at the def's formal_in, though @entry → the
		// use is also the use base's own edge: req (a value param whose
		// address is taken) and the value global GReq (&GReq in takeGReq)
		// reach q.UserID.
		{"AliasEntryValue", []PortEdge{pt("@formal_in:0", "457:2", "q.UserID"), pt("@formal_in:0", "@formal_out:1", "q")}},
		{"AliasEntryGlobal", []PortEdge{pt("@formal_in:1", "469:2", "q.UserID"), pt("@formal_in:1", "@formal_out:1", "q")}},
		// N2: an address-taken reference has its storage anchor too.
		{"AliasPtrPtr", []PortEdge{pt("474:2", "476:2", "pp"), pt("476:2", "477:2", "p.n")}},
		{"AliasSlicePtr", []PortEdge{pt("483:2", "484:2", "s[*]"), pt("483:2", "@formal_out:1", "s"), pt("@formal_in:0", "483:2", "ps")}},
		// O1: a slice of an array pointer aliases it.
		{"AliasArrPtr", []PortEdge{pt("490:2", "491:2", "pa[*]"), pt("490:2", "@formal_out:1", "pa")}},
	} {
		if got := pointsTo(res.Callables[sdgKey(c.fn)].Ddg); !slices.Equal(got, c.want) {
			t.Errorf("%s points-to edges:\n got  %+v\n want %+v", c.fn, got, c.want)
		}
	}
}

// TestAlias_ValueCopiesNeverAlias: AliasValue copies req by value (no
// reference inside Req) and AliasValueCopies copies s twice, no address
// taken: no points-to edge. The oracle table pins the type rules (typeOracle's
// doc; fix round 1, I1–I3).
func TestAlias_ValueCopiesNeverAlias(t *testing.T) {
	res := runAlias(t, true)
	for _, fn := range []string{"AliasValue", "AliasValueCopies"} {
		if got := pointsTo(res.Callables[sdgKey(fn)].Ddg); len(got) != 0 {
			t.Errorf("%s has points-to edges %+v", fn, got)
		}
	}

	pkg := types.NewPackage("example.com/p", "p")
	newNamed := func(name string, u types.Type) *types.Named {
		return types.NewNamed(types.NewTypeName(token.NoPos, pkg, name, nil), u, nil)
	}
	field := func(name string, t types.Type) *types.Var { return types.NewField(token.NoPos, pkg, name, t, false) }
	S := newNamed("S", types.NewStruct([]*types.Var{field("f", types.Typ[types.Int])}, nil))
	S.AddMethod(types.NewFunc(token.NoPos, pkg, "M", types.NewSignatureType(types.NewVar(token.NoPos, pkg, "s", types.NewPointer(S)), nil, nil, nil, nil, false)))
	method := func(name string) *types.Interface {
		return types.NewInterfaceType([]*types.Func{types.NewFunc(token.NoPos, pkg, name, types.NewSignatureType(nil, nil, nil, nil, nil, false))}, nil).Complete()
	}
	I, J := newNamed("I", method("M")), newNamed("J", method("N"))
	Inner := newNamed("Inner", types.NewStruct([]*types.Var{field("n", types.Typ[types.Int])}, nil))
	Outer := newNamed("Outer", types.NewStruct([]*types.Var{types.NewField(token.NoPos, pkg, "Inner", Inner, true)}, nil))
	bytesT := types.NewSlice(types.Typ[types.Byte])
	Bytes := newNamed("Bytes", bytesT)
	header := types.NewMap(types.Typ[types.String], types.NewSlice(types.Typ[types.String]))
	Header := newNamed("Header", header)
	ch := func(d types.ChanDir) types.Type { return types.NewChan(d, types.Typ[types.Int]) }
	fn := types.NewSignatureType(nil, nil, nil, types.NewTuple(types.NewVar(token.NoPos, pkg, "", types.Typ[types.Int])), nil, false)
	arr := types.NewArray(types.Typ[types.Int], 4)

	o := newTypeOracle(dataflow.Unit{}, nil).(*typeOracle)
	vars := map[string]*types.Var{}
	P := func(name string, t types.Type, spell string) dataflow.Path {
		v, ok := vars[name]
		if !ok {
			v = types.NewVar(token.NoPos, pkg, name, t)
			vars[name] = v
		}
		return dataflow.Path{Base: v, Spell: spell}
	}
	gv := types.NewVar(token.NoPos, pkg, "G", types.NewPointer(S))
	pkg.Scope().Insert(gv)
	gs := types.NewVar(token.NoPos, pkg, "GS", S)
	pkg.Scope().Insert(gs)
	taken := P("x", S, "x.f")
	o.taken[taken.Base] = true
	tarr := P("arr", arr, "arr[*]")
	o.taken[tarr.Base] = true
	o.takenSub[subKey{tarr.Base, "[*]"}] = true
	o.taken[P("tp", types.NewPointer(S), "tp").Base] = true
	to := P("o", Outer, "o.Inner.n")
	o.taken[to.Base] = true
	o.takenSub[subKey{to.Base, ".Inner"}] = true

	for _, c := range []struct {
		what string
		d, u dataflow.Path
		want bool
	}{
		{"value structs", P("a1", S, "a1.f"), P("b1", S, "b1.f"), false},
		{"ints", P("a2", types.Typ[types.Int], "a2"), P("b2", types.Typ[types.Int], "b2"), false},
		{"identical pointers", P("a3", types.NewPointer(S), "a3.f"), P("b3", types.NewPointer(S), "b3.f"), true},
		{"bare def of a pointer rebinds it", P("a3", nil, "a3"), P("b3", nil, "b3.f"), false},
		{"pointer, untaken value", P("a3", nil, "a3.f"), P("b4", S, "b4.f"), false},
		{"different pointees", P("a3", nil, "a3.f"), P("b5", types.NewPointer(types.Typ[types.Int]), "b5.*"), false},
		{"*S implements I", P("a3", nil, "a3.f"), P("i1", I, "i1.*"), true},
		{"I, *S", P("i1", nil, "i1.*"), P("a3", nil, "a3.f"), true},
		{"value S, I (only *S implements)", P("a1", nil, "a1.f"), P("i1", nil, "i1.*"), false},
		{"*S does not implement J", P("a3", nil, "a3.f"), P("j1", J, "j1.*"), false},
		{"I2: two unrelated interfaces", P("i1", nil, "i1.*"), P("j1", nil, "j1.*"), true},
		{"I1: Bytes vs []byte", P("s1", Bytes, "s1[*]"), P("s2", bytesT, "s2[*]"), true},
		{"I1: Header vs map[string][]string", P("h1", Header, "h1[*]"), P("h2", header, "h2[*]"), true},
		{"I1: chan int vs <-chan int", P("c1", ch(types.SendRecv), "c1[*]"), P("c2", ch(types.RecvOnly), "c2[*]"), true},
		{"I1: chan<- int vs chan int", P("c3", ch(types.SendOnly), "c3[*]"), P("c1", nil, "c1[*]"), true},
		{"I1: []int vs []string", P("s3", types.NewSlice(types.Typ[types.Int]), "s3[*]"), P("s4", types.NewSlice(types.Typ[types.String]), "s4[*]"), false},
		{"I1: identical funcs", P("f1", fn, "f1.*"), P("f2", fn, "f2.*"), true},
		{"O1: *[4]int vs []int", P("pa", types.NewPointer(arr), "pa[*]"), P("s3", nil, "s3[*]"), true},
		{"O1: []int vs *[4]int", P("s3", nil, "s3[*]"), P("pa", nil, "pa[*]"), true},
		{"O1: *[4]int vs []string", P("pa", nil, "pa[*]"), P("s4", nil, "s4[*]"), false},
		{"N2: taken pointer's storage, bare def", P("tp", types.NewPointer(S), "tp"), P("pp", types.NewPointer(types.NewPointer(S)), "pp.*"), true},
		{"global and local pointer", dataflow.Path{Base: gv, Spell: "example.com/p::G.f"}, P("a3", nil, "a3.f"), true},
		{"I3: *S write, taken S read", P("a3", nil, "a3.f"), taken, true},
		{"I3: taken S write, *S read", P("x", nil, "x.f"), P("a3", nil, "a3.f"), true},
		{"I3: taken S bare def, *S read", P("x", nil, "x"), P("a3", nil, "a3.f"), true},
		{"I3: global value is always taken", P("a3", nil, "a3.f"), dataflow.Path{Base: gs, Spell: "example.com/p::GS.f"}, true},
		{"I3: two taken values never share storage", P("x", nil, "x.f"), dataflow.Path{Base: gs, Spell: "example.com/p::GS.f"}, false},
		{"I3: sliced array vs []int", P("s3", nil, "s3[*]"), tarr, true},
		{"I3: &o.Inner offset, p.n vs o.Inner.n", P("p1", types.NewPointer(Inner), "p1.n"), to, true},
		{"I3: &o.Inner offset, reverse", to, P("p1", nil, "p1.n"), true},
		{"I3: *Inner vs untaken Outer field", P("p1", nil, "p1.n"), P("o2", Outer, "o2.Inner.n"), false},
		{"ref field: m vs s.m", P("m1", Header, "m1[*]"), P("hs", newNamed("HS", types.NewStruct([]*types.Var{field("m", header)}, nil)), "hs.m[*]"), true},
		{"ref field: bare read of the struct", P("m1", nil, "m1[*]"), P("hs", nil, "hs"), true},
		{"ref field: field rebind writes no target", P("hs", nil, "hs.m"), P("m1", nil, "m1[*]"), false},
		{"nil base", dataflow.Path{Spell: "u.f"}, P("a3", nil, "a3.f"), false},
		{"pseudo path", dataflow.Path{Spell: "<defer>:9:9"}, P("a3", nil, "a3.f"), false},
	} {
		if got := o.MayAlias(c.d, c.u); got != c.want {
			t.Errorf("%s: MayAlias(%s, %s) = %v, want %v", c.what, c.d.Spell, c.u.Spell, got, c.want)
		}
	}
}

// TestAlias_AddsOnly: the alias pass only adds. With the oracle off vs on,
// Run's Result is identical except that (rulings, Task 17) the on run's
// callable ddg holds the points-to edges besides the off run's edges, which
// it keeps unchanged, and its summary edges are a superset (the flows read
// the points-to edges, spec §4.9). No points-to edge repeats the (src, dst,
// var) of an L3 or reaching-defs edge, every endpoint is a body node or a
// vertex of the callable, no summary edge is global → global (#30), and
// skipped units get none.
func TestAlias_AddsOnly(t *testing.T) {
	in := loadInput(t, "dataflow")
	defer func(v func(dataflow.Unit, ast.Node) AliasOracle) { newAliasOracle = v }(newAliasOracle)
	newAliasOracle = nil
	off := Run(in)
	newAliasOracle = newTypeOracle
	on := Run(loadInput(t, "dataflow"))

	var nPT, nSumm int
	for k, c := range on.Callables {
		o := off.Callables[k]
		var rest []PortEdge
		for _, e := range c.Ddg {
			if e.Prov != ProvPointsTo {
				rest = append(rest, e)
			}
		}
		if !slices.Equal(rest, o.Ddg) {
			t.Errorf("%s: non-points-to ddg changed:\n off %+v\n on  %+v", k.Signature, o.Ddg, rest)
		}
		for _, e := range o.Summary {
			if !slices.Contains(c.Summary, e) {
				t.Errorf("%s: summary edge %+v lost", k.Signature, e)
			}
		}
		nSumm += len(c.Summary) - len(o.Summary)
		if !reflect.DeepEqual(c.Vertices, o.Vertices) {
			t.Errorf("%s: vertices changed", k.Signature)
		}
		of := map[string]string{}
		for _, v := range c.Vertices {
			of[v.Key] = v.Of
		}
		for _, e := range c.Summary {
			if strings.HasPrefix(of[e.Src], ofGlobal) && strings.HasPrefix(of[e.Dst], ofGlobal) {
				t.Errorf("%s: global → global summary %+v", k.Signature, e)
			}
		}
		ends := map[string]bool{}
		l3 := map[dataflow.DdgEdge]bool{}
		if r, ok := in.L3[k]; ok {
			for _, n := range r.Nodes {
				ends[n.Key] = true
			}
			for _, e := range r.Ddg {
				l3[e] = true
			}
		}
		for _, v := range c.Vertices {
			ends[v.Key] = true
		}
		rd := map[dataflow.DdgEdge]bool{}
		for _, e := range rest {
			rd[dataflow.DdgEdge{Src: e.Src, Dst: e.Dst, Var: e.Var}] = true
		}
		for _, e := range pointsTo(c.Ddg) {
			nPT++
			t3 := dataflow.DdgEdge{Src: e.Src, Dst: e.Dst, Var: e.Var}
			if l3[t3] || rd[t3] {
				t.Errorf("%s: points-to %+v repeats an L3 or reaching-defs edge", k.Signature, e)
			}
			if !ends[e.Src] || !ends[e.Dst] || e.Dst == dataflow.ExitKey || e.Src == dataflow.EntryKey {
				t.Errorf("%s: points-to %+v dangles or touches @entry/@exit", k.Signature, e)
			}
		}
		for i := 1; i < len(c.Ddg); i++ {
			if comparePortEdge(c.Ddg[i-1], c.Ddg[i]) >= 0 {
				t.Errorf("%s: ddg not sorted/unique at %+v", k.Signature, c.Ddg[i])
			}
		}
	}
	if nPT == 0 || nSumm == 0 {
		t.Fatalf("alias pass added %d points-to and %d summary edges: the gate covers nothing", nPT, nSumm)
	}
	t.Logf("dataflow: %d points-to edges, %d summary edges added", nPT, nSumm)

	off.Callables, on.Callables = nil, nil
	if !reflect.DeepEqual(off, on) {
		t.Errorf("Run's result outside Callables differs with the alias pass on")
	}
}

// TestAlias_SummaryThroughAlias: aliasAssign's v reaches its p only through
// the alias q (q := p; q.n = v), so its caller CallAliasAssign gains the
// summary edge from v's actual_in to p's actual_out with the pass on, and
// lacks it off.
func TestAlias_SummaryThroughAlias(t *testing.T) {
	want := SummaryEdge{Src: "382:2/actual_in:1", Dst: "382:2/actual_out"}
	if got := runAlias(t, true).Callables[sdgKey("CallAliasAssign")].Summary; !slices.Contains(got, want) {
		t.Errorf("CallAliasAssign summary %+v lacks %+v", got, want)
	}
	if got := runAlias(t, false).Callables[sdgKey("CallAliasAssign")].Summary; slices.Contains(got, want) {
		t.Errorf("CallAliasAssign summary %+v has %+v without the alias pass", got, want)
	}
}

// TestAlias_SkippedAndNoVTA: an l3_skipped unit gets no points-to edge (its
// pass-through wiring is already all-to-all), and under no_vta the pass
// still runs (it is intraprocedural; it reads no SSA).
func TestAlias_SkippedAndNoVTA(t *testing.T) {
	in := loadInput(t, "dataflow")
	delete(in.L3, sdgKey("AliasPtr"))
	res := Run(in)
	if got := pointsTo(res.Callables[sdgKey("AliasPtr")].Ddg); len(got) != 0 {
		t.Errorf("skipped AliasPtr has points-to edges %+v", got)
	}

	in = loadInput(t, "dataflow")
	defer func(f func(map[*ssa.Function]bool, *callgraph.Graph) *callgraph.Graph) { vtaCallGraph = f }(vtaCallGraph)
	vtaCallGraph = func(map[*ssa.Function]bool, *callgraph.Graph) *callgraph.Graph { panic("vta boom") }
	res = Run(in)
	if len(res.Degraded) == 0 || res.Degraded[len(res.Degraded)-1].Code != CodeNoVTA {
		t.Fatalf("degraded = %+v, want no_vta", res.Degraded)
	}
	if got := pointsTo(res.Callables[sdgKey("AliasPtr")].Ddg); !slices.Contains(got, pt("360:2", "361:2", "req.UserID")) {
		t.Errorf("no_vta AliasPtr points-to edges %+v lack 360:2 → 361:2", got)
	}
}

// TestWiring_GoldenWithAliasOff (M5) pins wire's output for every sdg
// fixture unit with the alias pass off, byte for byte, against
// testdata/wire_sdg_off.golden, which was generated by the pre-refactor
// wiring (7ec77e7) over this fixture: the port() refactor of the wiring is
// output-neutral. Regenerate with -update only for an intended change.
func TestWiring_GoldenWithAliasOff(t *testing.T) {
	defer func(v func(dataflow.Unit, ast.Node) AliasOracle) { newAliasOracle = v }(newAliasOracle)
	newAliasOracle = nil
	in, _, p := runPorts(t)
	var b strings.Builder
	for _, u := range in.Units {
		if u.Key.Path != sdgFile {
			continue
		}
		fmt.Fprintf(&b, "%s\n", u.Key.Signature)
		for _, e := range p.wire(u.Key) {
			fmt.Fprintf(&b, "\t%s -> %s %s %s\n", e.Src, e.Dst, e.Var, e.Prov)
		}
	}
	golden := filepath.Join("testdata", "wire_sdg_off.golden")
	if *update {
		if err := os.WriteFile(golden, []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if b.String() != string(want) {
		t.Errorf("wire output (alias off) differs from %s; diff with -update", golden)
	}
}
