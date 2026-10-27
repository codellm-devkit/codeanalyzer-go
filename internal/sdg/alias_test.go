package sdg

import (
	"go/token"
	"go/types"
	"reflect"
	"slices"
	"strings"
	"testing"

	"golang.org/x/tools/go/callgraph"
	"golang.org/x/tools/go/ssa"

	"github.com/codellm-devkit/codeanalyzer-go/internal/dataflow"
)

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

// runAlias runs L4 over testdata/dataflow with the alias oracle o (nil: off).
func runAlias(t *testing.T, o AliasOracle) Result {
	t.Helper()
	defer func(v AliasOracle) { aliasOracle = v }(aliasOracle)
	aliasOracle = o
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
func TestAlias_PointerCopyLinks(t *testing.T) {
	res := runAlias(t, typeOracle{})
	for _, c := range []struct {
		fn   string
		want []PortEdge
	}{
		{"AliasPtr", []PortEdge{pt("360:2", "361:2", "req.UserID"), pt("360:2", "@formal_out:1", "req")}},
		{"AliasPtrWhole", []PortEdge{pt("368:2", "369:2", "req"), pt("368:2", "@formal_out:1", "req")}},
		// aliasAssign (q := p; q.n = v): q.n = v reaches p's formal_out.
		{"aliasAssign", []PortEdge{pt("153:2", "@formal_out", "p")}},
	} {
		if got := pointsTo(res.Callables[sdgKey(c.fn)].Ddg); !slices.Equal(got, c.want) {
			t.Errorf("%s points-to edges:\n got  %+v\n want %+v", c.fn, got, c.want)
		}
	}
}

// TestAlias_ValueCopiesNeverAlias: AliasValue copies req by value (no
// reference inside Req), so the write to the copy reaches nothing through
// req: no points-to edge. The oracle itself refuses value types, nil bases
// and pseudo paths, and accepts an interface its operand implements (T or
// *T), a global pointer with a local one, and identical reference types.
func TestAlias_ValueCopiesNeverAlias(t *testing.T) {
	res := runAlias(t, typeOracle{})
	if got := pointsTo(res.Callables[sdgKey("AliasValue")].Ddg); len(got) != 0 {
		t.Errorf("AliasValue has points-to edges %+v", got)
	}

	pkg := types.NewPackage("example.com/p", "p")
	v := func(name string, t types.Type) dataflow.Path {
		return dataflow.Path{Base: types.NewVar(token.NoPos, pkg, name, t), Spell: name + ".f"}
	}
	st := types.NewStruct([]*types.Var{types.NewField(token.NoPos, pkg, "f", types.Typ[types.Int], false)}, nil)
	named := types.NewNamed(types.NewTypeName(token.NoPos, pkg, "S", nil), st, nil)
	m := types.NewFunc(token.NoPos, pkg, "M", types.NewSignatureType(types.NewVar(token.NoPos, pkg, "s", types.NewPointer(named)), nil, nil, nil, nil, false))
	named.AddMethod(m)
	iface := types.NewInterfaceType([]*types.Func{types.NewFunc(token.NoPos, pkg, "M", types.NewSignatureType(nil, nil, nil, nil, nil, false))}, nil).Complete()
	other := types.NewInterfaceType([]*types.Func{types.NewFunc(token.NoPos, pkg, "N", types.NewSignatureType(nil, nil, nil, nil, nil, false))}, nil).Complete()
	gv := types.NewVar(token.NoPos, pkg, "G", types.NewPointer(named))
	pkg.Scope().Insert(gv)
	global := dataflow.Path{Base: gv, Spell: "example.com/p::G.f"}

	o := typeOracle{}
	for _, c := range []struct {
		what string
		a, b dataflow.Path
		want bool
	}{
		{"value structs", v("a", named), v("b", named), false},
		{"ints", v("a", types.Typ[types.Int]), v("b", types.Typ[types.Int]), false},
		{"identical pointers", v("a", types.NewPointer(named)), v("b", types.NewPointer(named)), true},
		{"pointer, value", v("a", types.NewPointer(named)), v("b", named), false},
		{"different pointers", v("a", types.NewPointer(named)), v("b", types.NewPointer(types.Typ[types.Int])), false},
		{"*S implements I", v("a", types.NewPointer(named)), v("b", iface), true},
		{"I, *S", v("a", iface), v("b", types.NewPointer(named)), true},
		{"S's *S implements I, but S is a value", v("a", named), v("b", iface), false},
		{"*S does not implement J", v("a", types.NewPointer(named)), v("b", other), false},
		{"global and local pointer", global, v("b", types.NewPointer(named)), true},
		{"nil base", dataflow.Path{Spell: "u.f"}, v("b", types.NewPointer(named)), false},
		{"pseudo path", dataflow.Path{Spell: "<defer>:9:9"}, v("b", types.NewPointer(named)), false},
	} {
		if got := o.MayAlias(c.a, c.b); got != c.want {
			t.Errorf("%s: MayAlias = %v, want %v", c.what, got, c.want)
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
	defer func(v AliasOracle) { aliasOracle = v }(aliasOracle)
	aliasOracle = nil
	off := Run(in)
	aliasOracle = typeOracle{}
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
	if got := runAlias(t, typeOracle{}).Callables[sdgKey("CallAliasAssign")].Summary; !slices.Contains(got, want) {
		t.Errorf("CallAliasAssign summary %+v lacks %+v", got, want)
	}
	if got := runAlias(t, nil).Callables[sdgKey("CallAliasAssign")].Summary; slices.Contains(got, want) {
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
