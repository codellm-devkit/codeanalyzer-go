package v2emit

// L4 conformance harness for the v2 emitter.
//
// buildL4 runs the -a 4 pipeline: the -a 3 one, then sdg.Run, whose VTA
// call_graph edges and VTA-only externals are merged into the application
// before the L4 payload is emitted (as core does), with the SDG overlay
// (vertices, port ddg, summaries, param edges, stand-in bodies, degraded).
// The monotonicity gate below holds over it (-a 3 ⊆ -a 4).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/codellm-devkit/codeanalyzer-go/internal/dataflow"
	"github.com/codellm-devkit/codeanalyzer-go/internal/schema"
	v2 "github.com/codellm-devkit/codeanalyzer-go/internal/schema/v2"
	"github.com/codellm-devkit/codeanalyzer-go/internal/sdg"
	"github.com/codellm-devkit/codeanalyzer-go/internal/semantic_analysis"
	"github.com/codellm-devkit/codeanalyzer-go/internal/syntactic_analysis"
)

type l4Fixture struct {
	st     map[string]schema.GoFile
	l3, l4 *v2.Analysis
	res    map[dataflow.L3Key]dataflow.Result
	sdg    *sdg.Result
	dir    string
}

var (
	l4Mu    sync.Mutex
	l4Cache = map[string]*l4Fixture{}
)

// buildL4 runs the -a 4 pipeline over testdata/<name> once and emits both the
// L3 and the L4 payload from the same resolved symbol table. The app name is
// name's last element (cango's default: the input directory's base name).
func buildL4(t *testing.T, name string) *l4Fixture {
	t.Helper()
	l4Mu.Lock()
	defer l4Mu.Unlock()
	if fx, ok := l4Cache[name]; ok {
		return fx
	}
	_, thisFile, _, _ := runtime.Caller(0)
	dir, err := filepath.Abs(filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	b := syntactic_analysis.NewSymbolTableBuilder(dir)
	st, err := b.Build(nil, true)
	if err != nil {
		t.Fatalf("symbol table build failed: %v", err)
	}
	cg := semantic_analysis.NewCallGraphBuilder(dir, b.Fset(), b.Pkgs())
	edges := cg.Build(st)
	app := &schema.GoApplication{SymbolTable: st, CallGraph: edges, Externals: cg.Externals()}
	units := b.L3Units(st)
	res := dataflow.RunWithSideTables(b.Fset(), units, 3, 4)
	appName := filepath.Base(dir)
	fx := &l4Fixture{st: st, l3: Emit(app, appName, dir, 3, "gate-test", 3, res, nil), res: res, dir: dir}
	l4 := sdg.Run(sdg.Input{
		Fset: b.Fset(), Pkgs: b.Pkgs(), ProjectDir: dir, SymbolTable: st,
		Externals: app.Externals, CallGraph: app.CallGraph,
		Units: units, L3: res, K: 3, Jobs: 4,
	})
	l4.ApplyTo(app)
	fx.sdg = &l4
	fx.l4 = Emit(app, appName, dir, 4, "gate-test", 3, res, &l4)
	l4Cache[name] = fx
	return fx
}

// supersetL4 records where a (-a 3) is not contained in b (-a 4): every object
// key and value, and every list entry, by position — except the sorted edge
// lists L4 adds entries into (call_graph gets VTA edges, a callable's ddg
// gets port edges), where every -a 3 edge must occur unchanged somewhere in
// the -a 4 list.
func supersetL4(path string, a, b any, errs *[]string) {
	if path == "$.application.call_graph" || strings.HasSuffix(path, ".ddg") {
		av, aok := a.([]any)
		bv, bok := b.([]any)
		if !aok || !bok {
			*errs = append(*errs, path+": changed type")
			return
		}
		have := map[string]bool{}
		for _, e := range bv {
			have[jsonKey(e)] = true
		}
		for _, e := range av {
			if !have[jsonKey(e)] {
				*errs = append(*errs, path+": edge "+jsonKey(e)+" missing or changed at L4")
			}
		}
		return
	}
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok {
			*errs = append(*errs, path+": object changed type")
			return
		}
		for k, x := range av {
			y, ok := bv[k]
			if !ok {
				*errs = append(*errs, path+"."+k+" missing at L4")
				continue
			}
			supersetL4(path+"."+k, x, y, errs)
		}
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) > len(bv) {
			*errs = append(*errs, path+": list shrank or changed type")
			return
		}
		for i := range av {
			supersetL4(path+"[]", av[i], bv[i], errs)
		}
	default:
		if a != b {
			*errs = append(*errs, path+": value changed")
		}
	}
}

// TestGateL4_SupersetOfL3 is the monotonicity gate: everything in the -a 3 JSON
// is present and equal in the -a 4 JSON. Only max_level, the level marker
// itself, may differ.
func TestGateL4_SupersetOfL3(t *testing.T) {
	for _, name := range l4Fixtures {
		fx := buildL4(t, name)
		a, b := toAny(t, fx.l3), toAny(t, fx.l4)
		if b["max_level"] != float64(4) {
			t.Errorf("%s: max_level = %v, want 4", name, b["max_level"])
		}
		a["max_level"], b["max_level"] = nil, nil
		var errs []string
		supersetL4("$", a, b, &errs)
		for i, e := range errs {
			if i == 20 {
				t.Errorf("%s: … %d more", name, len(errs)-20)
				break
			}
			t.Errorf("%s: %s", name, e)
		}
	}
}

func jsonKey(v any) string {
	data, _ := json.Marshal(v)
	return string(data)
}

// TestGateL4_CallGraphHasVTAProv: at -a 4 the call graph gains VTA's dispatch
// edges (spec §4.8), prov ["vta"], weight 1, each one new: the concrete
// implementations of an interface call, the function passed to a func-value
// call, a promoted method, and a library method only VTA reaches, whose
// external (with its receiver) joins external_symbols. Every endpoint
// resolves, and the L2 edges are unchanged (the superset gate).
func TestGateL4_CallGraphHasVTAProv(t *testing.T) {
	fx := buildL4(t, "dataflow")
	app := fx.l4.Application
	mod := v2.ModuleID(app.ID, "sdg/sdg.go")
	fn := func(sig string) string {
		return v2.CallableID(mod, "example.com/dataflow/sdg."+sig)
	}
	method := func(typ, m string) string {
		return v2.CallableID(v2.TypeID(mod, "example.com/dataflow/sdg."+typ), "example.com/dataflow/sdg."+typ+"."+m)
	}
	bufWrite := v2.ExternalID(app.ID, "bytes", "Buffer.Write")
	want := [][2]string{
		{fn("UseStore"), method("memStore", "Get")},
		{fn("UseStore"), method("sqlStore", "Get")},
		{fn("CallFuncValue"), fn("double")},
		{fn("CallPromoted"), method("Inner", "M")},
		{fn("Buffered"), bufWrite},
	}
	l3 := map[[2]string]bool{}
	for _, e := range fx.l3.Application.CallGraph {
		l3[[2]string{e.Src, e.Dst}] = true
	}
	vta := map[[2]string]v2.Edge{}
	for _, e := range app.CallGraph {
		if slices.Contains(e.Prov, "vta") {
			k := [2]string{e.Src, e.Dst}
			if !slices.Equal(e.Prov, []string{"vta"}) || e.Weight != 1 || l3[k] {
				t.Errorf("VTA edge %+v: want prov [vta], weight 1, not an L3 edge", e)
			}
			vta[k] = e
		}
	}
	for _, k := range want {
		if _, ok := vta[k]; !ok {
			t.Errorf("no VTA edge %s -> %s", k[0], k[1])
		}
	}
	if l3[[2]string{fn("UseStore"), method("Store", "Get")}] == false {
		t.Errorf("L2 edge UseStore -> Store.Get missing")
	}
	if _, ok := fx.l3.Application.ExternalSymbols[bufWrite]; ok {
		t.Fatalf("-a 3 already has %s; the fixture no longer pins a VTA-only external", bufWrite)
	}
	ext, ok := app.ExternalSymbols[bufWrite]
	if !ok {
		t.Fatalf("external_symbols lack %s", bufWrite)
	}
	if ext.Receiver == nil || ext.Receiver.Name != "b" || ext.Receiver.Type != "*bytes.Buffer" || ext.Signature != "Buffer.Write" {
		t.Errorf("%s = %+v (receiver %+v)", bufWrite, ext, ext.Receiver)
	}
	// Each file's init closure is its own target: the edges keep both.
	initClosure := func(file string) string {
		m := v2.ModuleID(app.ID, "initclosure/"+file)
		init := v2.CallableID(m, "example.com/dataflow/initclosure.init")
		return v2.CallableID(init, "example.com/dataflow/initclosure.init.closure_1")
	}
	run := v2.CallableID(v2.ModuleID(app.ID, "initclosure/a.go"), "example.com/dataflow/initclosure.Run")
	for _, file := range []string{"a.go", "b.go"} {
		if _, ok := vta[[2]string{run, initClosure(file)}]; !ok {
			t.Errorf("no VTA edge Run -> %s's init closure", file)
		}
	}
	for _, name := range []string{"dataflow", "chi", "generics"} {
		assertNoDanglingEdges(t, buildL4(t, name).l4)
	}
}

// ─── the L4 overlay in the tree (Task 16) ────────────────────────────────────

// l4Fixtures are the fixtures the L4 tree gates run over.
var l4Fixtures = []string{"dataflow", "chi", "generics", "storedlit"}

// isVertexKind reports an L4 port vertex kind.
func isVertexKind(k string) bool {
	switch sdg.VertexKind(k) {
	case sdg.KindFormalIn, sdg.KindFormalOut, sdg.KindActualIn, sdg.KindActualOut:
		return true
	}
	return false
}

// l4Callables is every callable of the tree (methods and closures included).
func l4Callables(a *v2.Analysis) []v2.Callable {
	var out []v2.Callable
	var walk func(c v2.Callable)
	walk = func(c v2.Callable) {
		out = append(out, c)
		for _, in := range c.Callables {
			walk(in)
		}
	}
	for _, m := range a.Application.SymbolTable {
		for _, ty := range m.Types {
			for _, c := range ty.Callables {
				walk(c)
			}
		}
		for _, f := range m.Functions {
			walk(f)
		}
	}
	return out
}

// bodyIndex maps every body node id of the tree — callables' and stand-ins'
// — to the node and the id of the callable or external that owns it.
type ownedNode struct {
	owner string
	key   string
	node  v2.BodyNode
}

func bodyIndex(a *v2.Analysis) map[string]ownedNode {
	idx := map[string]ownedNode{}
	for _, c := range l4Callables(a) {
		for k, n := range c.Body {
			idx[n.ID] = ownedNode{c.ID, k, n}
		}
	}
	for id, ext := range a.Application.ExternalSymbols {
		for k, n := range ext.Body {
			idx[n.ID] = ownedNode{id, k, n}
		}
	}
	return idx
}

// TestGateL4_NoDanglingEndpoints: every L4 edge endpoint is a node of the
// tree — ddg and summary endpoints are body keys of their callable, param
// edge endpoints are body node ids (a callable's or a stand-in's) — every
// degraded callable is a callable id, and nothing the SDG computed is lost on
// the way: the tree carries every vertex, port edge, summary edge, param
// edge and stand-in formal of the sdg.Result.
func TestGateL4_NoDanglingEndpoints(t *testing.T) {
	// rangefunc: the one fixture with degraded callables, so a dropped one
	// fails the degraded rows of the table below.
	for _, name := range append(slices.Clone(l4Fixtures), "rangefunc") {
		fx := buildL4(t, name)
		a := fx.l4
		idx := bodyIndex(a)
		ids := allCallableIDs(a)
		var vertices, port, summary int
		for _, c := range l4Callables(a) {
			for k, n := range c.Body {
				if isVertexKind(n.Kind) {
					vertices++
				}
				if n.Parent != "" {
					if _, ok := c.Body[n.Parent]; !ok {
						t.Errorf("%s: %s: body %s parent %q is not a body key", name, c.ID, k, n.Parent)
					}
				}
			}
			for _, e := range c.Ddg {
				for _, k := range []string{e.Src, e.Dst} {
					if _, ok := c.Body[k]; !ok {
						t.Errorf("%s: %s: ddg endpoint %q is not a body key", name, c.ID, k)
					}
				}
				if !slices.Equal(e.Prov, []string{"ssa"}) {
					port++
				}
			}
			for _, e := range c.Summary {
				for _, k := range []string{e.Src, e.Dst} {
					if n, ok := c.Body[k]; !ok || !isVertexKind(n.Kind) {
						t.Errorf("%s: %s: summary endpoint %q is not a vertex", name, c.ID, k)
					}
				}
			}
			summary += len(c.Summary)
		}
		for list, edges := range map[string][]v2.ParamEdge{"param_in": a.Application.ParamIn, "param_out": a.Application.ParamOut} {
			for _, e := range edges {
				for _, id := range []string{e.Src, e.Dst} {
					if _, ok := idx[id]; !ok {
						t.Errorf("%s: %s endpoint %s is not a body node", name, list, id)
					}
				}
			}
		}
		for _, d := range a.Degraded {
			for _, id := range d.Callables {
				if !ids[id] {
					t.Errorf("%s: degraded %s names %s, not a callable id", name, d.Code, id)
				}
			}
		}

		var wantV, wantP, wantS, wantX, wantD, gotD int
		for _, d := range fx.sdg.Degraded {
			ks := slices.Clone(d.Callables)
			slices.SortFunc(ks, func(a, b dataflow.L3Key) int { return cmpStrings(a.Path, b.Path, a.Signature, b.Signature) })
			wantD += len(slices.Compact(ks))
		}
		for _, d := range a.Degraded {
			gotD += len(d.Callables)
		}
		for _, c := range fx.sdg.Callables {
			wantV, wantP, wantS = wantV+len(c.Vertices), wantP+len(c.Ddg), wantS+len(c.Summary)
		}
		gotX := 0
		for _, ext := range a.Application.ExternalSymbols {
			gotX += len(ext.Body)
		}
		for _, vs := range fx.sdg.Externals {
			wantX += len(vs)
		}
		for _, c := range []struct {
			what      string
			got, want int
		}{
			{"vertices", vertices, wantV},
			{"port ddg edges", port, wantP},
			{"summary edges", summary, wantS},
			{"param_in", len(a.Application.ParamIn), len(fx.sdg.ParamIn)},
			{"param_out", len(a.Application.ParamOut), len(fx.sdg.ParamOut)},
			{"stand-in formals", gotX, wantX},
			{"degraded entries", len(a.Degraded), len(fx.sdg.Degraded)},
			{"degraded callables", gotD, wantD},
		} {
			if c.got != c.want {
				t.Errorf("%s: tree has %d %s, sdg.Result %d", name, c.got, c.what, c.want)
			}
		}
		if name != "storedlit" && name != "rangefunc" && (wantV == 0 || len(fx.sdg.ParamIn) == 0 || wantS == 0) {
			t.Errorf("%s: empty overlay (vertices %d, param_in %d, summary %d)", name, wantV, len(fx.sdg.ParamIn), wantS)
		}
		if name == "rangefunc" && wantD == 0 {
			t.Errorf("rangefunc: no degraded callable")
		}
		t.Logf("%s: %d vertices, %d port ddg, %d summary, %d param_in, %d param_out, %d stand-in formals, %d degraded callables",
			name, vertices, port, summary, len(a.Application.ParamIn), len(a.Application.ParamOut), gotX, gotD)
	}
}

// TestGateL4_VertexIdsAreGlobalOrdinals: every vertex — a callable's or a
// stand-in's — has id v2.GlobalOrdinal(owner id, key), a kind, an `of`, no
// span and no callee key; only actual vertices carry a parent. A stand-in
// vertex id is <external-id>@formal_in:<i> (one "@" after the external id).
func TestGateL4_VertexIdsAreGlobalOrdinals(t *testing.T) {
	for _, name := range l4Fixtures {
		fx := buildL4(t, name)
		for id, on := range bodyIndex(fx.l4) {
			n := on.node
			if want := v2.GlobalOrdinal(on.owner, on.key); id != want {
				t.Errorf("%s: body id %s, want %s", name, id, want)
			}
			if !isVertexKind(n.Kind) {
				if n.Of != "" {
					t.Errorf("%s: non-vertex %s carries of %q", name, id, n.Of)
				}
				continue
			}
			if n.Of == "" || n.Span != (v2.Span{}) || n.Callee != nil {
				t.Errorf("%s: vertex %s = %+v, want an of, no span, no callee", name, id, n)
			}
			actual := n.Kind == string(sdg.KindActualIn) || n.Kind == string(sdg.KindActualOut)
			if actual != (n.Parent != "") {
				t.Errorf("%s: vertex %s kind %s parent %q", name, id, n.Kind, n.Parent)
			}
			data, err := json.Marshal(n)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), `"callee"`) || strings.Contains(string(data), `"span"`) {
				t.Errorf("%s: vertex %s serialises %s", name, id, data)
			}
		}
		for id, ext := range fx.l4.Application.ExternalSymbols {
			for k := range ext.Body {
				if !strings.HasPrefix(k, "@formal_") || strings.Count(ext.Body[k].ID[len(id):], "@") != 1 {
					t.Errorf("%s: stand-in %s body key %s id %s", name, id, k, ext.Body[k].ID)
				}
			}
		}
	}
}

// TestGateL4_ArityMatchesFormals: every param_in runs from an actual_in of
// the caller to a formal_in that exists in the target's body (a callable or
// a stand-in), and every param_out from such a formal_out to an actual_out;
// var is the target formal's `of` on both.
func TestGateL4_ArityMatchesFormals(t *testing.T) {
	for _, name := range l4Fixtures {
		fx := buildL4(t, name)
		idx := bodyIndex(fx.l4)
		check := func(list string, e v2.ParamEdge, actual, formal string, formalEnd string) {
			src, dst := idx[e.Src], idx[e.Dst]
			act, form := src, dst
			if formalEnd == "src" {
				act, form = dst, src
			}
			if act.node.Kind != actual || form.node.Kind != formal {
				t.Errorf("%s: %s %s -> %s joins %s to %s", name, list, e.Src, e.Dst, src.node.Kind, dst.node.Kind)
			}
			if e.Var != form.node.Of {
				t.Errorf("%s: %s %s -> %s var %q, target formal of %q", name, list, e.Src, e.Dst, e.Var, form.node.Of)
			}
		}
		for _, e := range fx.l4.Application.ParamIn {
			check("param_in", e, string(sdg.KindActualIn), string(sdg.KindFormalIn), "dst")
		}
		for _, e := range fx.l4.Application.ParamOut {
			check("param_out", e, string(sdg.KindActualOut), string(sdg.KindFormalOut), "src")
		}
	}
}

// TestGateL4_ParamEdgesSorted: param_in and param_out are sorted by
// (src, dst, var) over the minted ids and hold no duplicate; each
// callable's ddg is sorted by (src, dst, var, prov) and its summary by
// (src, dst).
func TestGateL4_ParamEdgesSorted(t *testing.T) {
	for _, name := range l4Fixtures {
		fx := buildL4(t, name)
		for list, edges := range map[string][]v2.ParamEdge{"param_in": fx.l4.Application.ParamIn, "param_out": fx.l4.Application.ParamOut} {
			for i := 1; i < len(edges); i++ {
				a, b := edges[i-1], edges[i]
				if cmpStrings(a.Src, b.Src, a.Dst, b.Dst, a.Var, b.Var) >= 0 {
					t.Errorf("%s: %s[%d..%d] out of order: %+v, %+v", name, list, i-1, i, a, b)
				}
			}
		}
		for _, c := range l4Callables(fx.l4) {
			for i := 1; i < len(c.Ddg); i++ {
				a, b := c.Ddg[i-1], c.Ddg[i]
				if cmpStrings(a.Src, b.Src, a.Dst, b.Dst, a.Var, b.Var, strings.Join(a.Prov, ","), strings.Join(b.Prov, ",")) >= 0 {
					t.Errorf("%s: %s ddg[%d..%d] out of order: %+v, %+v", name, c.ID, i-1, i, a, b)
				}
			}
			for i := 1; i < len(c.Summary); i++ {
				a, b := c.Summary[i-1], c.Summary[i]
				if cmpStrings(a.Src, b.Src, a.Dst, b.Dst) >= 0 {
					t.Errorf("%s: %s summary[%d..%d] out of order", name, c.ID, i-1, i)
				}
			}
		}
	}
}

// cmpStrings compares the pairs (xs[0], xs[1]), (xs[2], xs[3]), … in turn.
func cmpStrings(xs ...string) int {
	for i := 0; i+1 < len(xs); i += 2 {
		if c := strings.Compare(xs[i], xs[i+1]); c != 0 {
			return c
		}
	}
	return 0
}

// TestGateL4_UseStoreEdges pins the emitted L4 of sdg.UseStore (its
// interface call s.Get at 34:9 binds Store.Get, memStore.Get and
// sqlStore.Get): its vertices, its all-to-all summary, and its param edges
// with the minted ids.
func TestGateL4_UseStoreEdges(t *testing.T) {
	fx := buildL4(t, "dataflow")
	app := fx.l4.Application
	mod := v2.ModuleID(app.ID, "sdg/sdg.go")
	useStore := v2.CallableID(mod, "example.com/dataflow/sdg.UseStore")
	method := func(typ, m string) string {
		return v2.CallableID(v2.TypeID(mod, "example.com/dataflow/sdg."+typ), "example.com/dataflow/sdg."+typ+"."+m)
	}
	var c v2.Callable
	for _, x := range l4Callables(fx.l4) {
		if x.ID == useStore {
			c = x
		}
	}
	for k, of := range map[string]string{
		"@formal_in:0": "s", "@formal_in:1": "k", "@formal_out:0": "<return>", "@formal_out:1": "s",
		"34:9/actual_in:0": "_recv", "34:9/actual_in:1": "k",
		"34:9/actual_out:0": "<return>", "34:9/actual_out:1": "_recv",
	} {
		n, ok := c.Body[k]
		if !ok || n.Of != of || n.ID != v2.GlobalOrdinal(useStore, k) {
			t.Errorf("UseStore body %s = %+v, want of %q", k, n, of)
		}
	}
	if len(c.Summary) != 4 {
		t.Errorf("UseStore summary = %v, want all-to-all over 2×2", c.Summary)
	}
	in := map[[3]string]bool{}
	for _, e := range app.ParamIn {
		in[[3]string{e.Src, e.Dst, e.Var}] = true
	}
	out := map[[3]string]bool{}
	for _, e := range app.ParamOut {
		out[[3]string{e.Src, e.Dst, e.Var}] = true
	}
	site := func(k string) string { return v2.GlobalOrdinal(useStore, "34:9/"+k) }
	for _, e := range [][3]string{
		{site("actual_in:0"), method("Store", "Get") + "@formal_in:0", "_recv"},
		{site("actual_in:0"), method("memStore", "Get") + "@formal_in:0", "s"},
		{site("actual_in:0"), method("sqlStore", "Get") + "@formal_in:0", "s"},
		{site("actual_in:1"), method("sqlStore", "Get") + "@formal_in:1", "k"},
	} {
		if !in[e] {
			t.Errorf("no param_in %v", e)
		}
	}
	for _, e := range [][3]string{
		{method("Store", "Get") + "@formal_out:0", site("actual_out:0"), "<return>"},
		{method("memStore", "Get") + "@formal_out:1", site("actual_out:1"), "s"},
	} {
		if !out[e] {
			t.Errorf("no param_out %v", e)
		}
	}
	stores := v2.CallableID(mod, "example.com/dataflow/sdg.Stores")
	if !in[[3]string{v2.GlobalOrdinal(stores, "40:2/actual_in:1"), useStore + "@formal_in:1", "k"}] {
		t.Errorf("no param_in Stores@40:2/actual_in:1 -> UseStore@formal_in:1")
	}
}

// TestGateL4_StandInBody: a library callee the project calls carries its
// stand-in formals in external_symbols[].body (spec §6), and a param edge
// reaches them.
func TestGateL4_StandInBody(t *testing.T) {
	fx := buildL4(t, "dataflow")
	app := fx.l4.Application
	id := v2.ExternalID(app.ID, "fmt", "Sprintf")
	ext, ok := app.ExternalSymbols[id]
	if !ok {
		t.Fatalf("no external %s", id)
	}
	// a ...any is a by-ref out (an interface slice), so formal_out is indexed.
	for k, of := range map[string]string{"@formal_in:0": "format", "@formal_in:1": "a", "@formal_out:0": "<return>", "@formal_out:1": "a"} {
		n, ok := ext.Body[k]
		if !ok || n.Of != of || n.ID != id+k {
			t.Errorf("%s body %s = %+v, want of %q", id, k, n, of)
		}
	}
	reached := false
	for _, e := range app.ParamIn {
		reached = reached || e.Dst == id+"@formal_in:0"
	}
	if !reached {
		t.Errorf("no param_in reaches %s@formal_in:0", id)
	}
	if _, ok := fx.l3.Application.ExternalSymbols[id]; !ok {
		t.Fatalf("-a 3 lacks %s", id)
	}
	if fx.l3.Application.ExternalSymbols[id].Body != nil {
		t.Errorf("-a 3 carries a stand-in body")
	}
}

// TestGateL4_DegradedMintsCallables: a degraded run's markers land on the
// envelope with each callable as its can:// id, sorted, entries by code.
func TestGateL4_DegradedMintsCallables(t *testing.T) {
	fx := buildL4(t, "rangefunc")
	deg := fx.l4.Degraded
	if len(deg) == 0 {
		t.Fatal("rangefunc: no degraded marker")
	}
	ids := allCallableIDs(fx.l4)
	found := false
	for i, d := range deg {
		if i > 0 && cmpStrings(deg[i-1].Code, d.Code, deg[i-1].Detail, d.Detail) > 0 {
			t.Errorf("degraded not sorted by code: %v", deg)
		}
		if !slices.IsSorted(d.Callables) {
			t.Errorf("%s callables not sorted: %v", d.Code, d.Callables)
		}
		for _, id := range d.Callables {
			if !ids[id] {
				t.Errorf("%s names %s, not a callable id", d.Code, id)
			}
		}
		if d.Code == sdg.CodeRangeFunc {
			found = true
			want := v2.CallableID(v2.ModuleID(fx.l4.Application.ID, "r.go"), "example.com/rangefunc.RangeFunc")
			if !slices.Contains(d.Callables, want) {
				t.Errorf("range_func callables %v lack %s", d.Callables, want)
			}
		}
	}
	if !found {
		t.Errorf("degraded %+v has no range_func", deg)
	}
	if fx.l3.Degraded != nil {
		t.Errorf("-a 3 carries degraded")
	}
	if len(buildL4(t, "dataflow").l4.Degraded) != 0 {
		t.Errorf("dataflow degraded = %+v, want none", buildL4(t, "dataflow").l4.Degraded)
	}
}

// TestGateL4_StoredLiteralGlobalReachable (Task 13 review): storedlit's
// Publish writes the global sink only inside a literal it stores, and Fire
// calls that literal through a function value nothing resolves — so inside
// Publish, sink's formal_out is fed only by its formal_in (the identity
// edge). The flow v → sink → readSink → <return> must still be reachable in
// the emitted graph (ddg ∪ cdg ∪ summary ∪ param_in ∪ param_out, the SDK's
// walk): the caller's host statement Publish(v) weak-defines sink, whose
// later use feeds readSink's global formal_in.
func TestGateL4_StoredLiteralGlobalReachable(t *testing.T) {
	fx := buildL4(t, "storedlit")
	a := fx.l4
	succ := map[string][]string{}
	for _, c := range l4Callables(a) {
		g := func(k string) string { return v2.GlobalOrdinal(c.ID, k) }
		for _, e := range c.Ddg {
			succ[g(e.Src)] = append(succ[g(e.Src)], g(e.Dst))
		}
		for _, e := range c.Cdg {
			succ[g(e.Src)] = append(succ[g(e.Src)], g(e.Dst))
		}
		for _, e := range c.Summary {
			succ[g(e.Src)] = append(succ[g(e.Src)], g(e.Dst))
		}
	}
	for _, e := range slices.Concat(a.Application.ParamIn, a.Application.ParamOut) {
		succ[e.Src] = append(succ[e.Src], e.Dst)
	}
	reach := func(from string) map[string]bool {
		seen := map[string]bool{from: true}
		for work := []string{from}; len(work) > 0; {
			n := work[len(work)-1]
			work = work[:len(work)-1]
			for _, m := range succ[n] {
				if !seen[m] {
					seen[m] = true
					work = append(work, m)
				}
			}
		}
		return seen
	}
	mod := v2.ModuleID(a.Application.ID, "storedlit.go")
	fn := func(name string) string { return v2.CallableID(mod, "example.com/storedlit."+name) }
	idx := bodyIndex(a)
	formal := func(owner, kind, of string) string {
		for id, on := range idx {
			if on.owner == owner && on.node.Kind == kind && on.node.Of == of {
				return id
			}
		}
		t.Fatalf("%s has no %s of %q", owner, kind, of)
		return ""
	}
	src := formal(fn("PublishThenRead"), "formal_in", "v")
	r := reach(src)
	for _, dst := range []string{
		formal(fn("readSink"), "formal_in", "<global>:example.com/storedlit::sink"),
		formal(fn("PublishThenRead"), "formal_out", "<return>"),
	} {
		if !r[dst] {
			t.Errorf("%s does not reach %s", src, dst)
		}
	}
	// The gap the caller covers: over Publish's own edges (ddg ∪ cdg ∪
	// summary), v does not reach sink's formal_out — sink's formal_out is fed
	// only by its formal_in, the identity edge, as the literal's write has no
	// param_out. Update this when the gap closes.
	var pub v2.Callable
	for _, c := range l4Callables(a) {
		if c.ID == fn("Publish") {
			pub = c
		}
	}
	local := map[string][]string{}
	for _, e := range pub.Ddg {
		local[e.Src] = append(local[e.Src], e.Dst)
	}
	for _, e := range pub.Cdg {
		local[e.Src] = append(local[e.Src], e.Dst)
	}
	for _, e := range pub.Summary {
		local[e.Src] = append(local[e.Src], e.Dst)
	}
	keyOf := func(id string) string { return idx[id].key }
	seen := map[string]bool{keyOf(formal(fn("Publish"), "formal_in", "v")): true}
	for work := []string{keyOf(formal(fn("Publish"), "formal_in", "v"))}; len(work) > 0; {
		n := work[len(work)-1]
		work = work[:len(work)-1]
		for _, m := range local[n] {
			if !seen[m] {
				seen[m] = true
				work = append(work, m)
			}
		}
	}
	sinkOut := keyOf(formal(fn("Publish"), "formal_out", "<global>:example.com/storedlit::sink"))
	if seen[sinkOut] {
		t.Errorf("Publish: v now reaches %s intra-procedurally; the stored-literal gap closed, update this test", sinkOut)
	}
	var feeders []string
	for _, e := range pub.Ddg {
		if e.Dst == sinkOut {
			feeders = append(feeders, e.Src)
		}
	}
	if !slices.Equal(feeders, []string{keyOf(formal(fn("Publish"), "formal_in", "<global>:example.com/storedlit::sink"))}) {
		t.Errorf("Publish: %s fed by %v, want only sink's formal_in (identity edge)", sinkOut, feeders)
	}
}

// buildL4Jobs runs the -a 4 pipeline over dir from scratch with -j jobs and
// returns EmitTo's bytes, checking they equal json.Marshal(Emit(...)).
func buildL4Jobs(t *testing.T, dir, name string, jobs int) []byte {
	t.Helper()
	b := syntactic_analysis.NewSymbolTableBuilder(dir)
	st, err := b.Build(nil, true)
	if err != nil {
		t.Fatalf("symbol table build failed: %v", err)
	}
	cg := semantic_analysis.NewCallGraphBuilder(dir, b.Fset(), b.Pkgs())
	edges := cg.Build(st)
	app := &schema.GoApplication{SymbolTable: st, CallGraph: edges, Externals: cg.Externals()}
	units := b.L3Units(st)
	res := dataflow.RunWithSideTables(b.Fset(), units, 3, jobs)
	l4 := sdg.Run(sdg.Input{
		Fset: b.Fset(), Pkgs: b.Pkgs(), ProjectDir: dir, SymbolTable: st,
		Externals: app.Externals, CallGraph: app.CallGraph,
		Units: units, L3: res, K: 3, Jobs: jobs,
	})
	l4.ApplyTo(app)
	dataflow.ReleaseAll(res)
	var buf bytes.Buffer
	if err := EmitTo(&buf, app, name, dir, 4, "det", 3, res, &l4); err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal(Emit(app, name, dir, 4, "det", 3, res, &l4))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("%s: EmitTo differs from json.Marshal(Emit) at byte %d (len %d vs %d)", name, firstDiff(buf.Bytes(), want), buf.Len(), len(want))
	}
	return buf.Bytes()
}

// TestGateL4_DeterministicAcrossJobs: the -a 4 JSON is byte-identical
// between -j 1 and -j 8 and across fresh runs, on sdg, flow and chi; the
// streaming writer matches json.Marshal of the built tree.
func TestGateL4_DeterministicAcrossJobs(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	td := filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "testdata")
	for name, rel := range map[string]string{
		"sdg":  filepath.Join("dataflow", "sdg"),
		"flow": filepath.Join("dataflow", "flow"),
		"chi":  "chi",
	} {
		dir, err := filepath.Abs(filepath.Join(td, rel))
		if err != nil {
			t.Fatal(err)
		}
		first := buildL4Jobs(t, dir, name, 1)
		if !bytes.Contains(first, []byte(`"param_in":[`)) {
			t.Errorf("%s: -a 4 JSON has no param_in", name)
		}
		for i, jobs := range []int{8, 1, 8} {
			if got := buildL4Jobs(t, dir, name, jobs); !bytes.Equal(got, first) {
				t.Errorf("%s: run %d (-j %d) differs from -j 1 at byte %d", name, i+2, jobs, firstDiff(got, first))
			}
		}
	}
}

// TestEmitL4_UnresolvedRefIsDroppedAndWarned: a param edge whose endpoint
// names no emitted callable is a bug upstream; the emitter never writes it
// with a dangling endpoint (it warns and drops it).
func TestEmitL4_UnresolvedRefIsDroppedAndWarned(t *testing.T) {
	fx := buildL4(t, "dataflow")
	res := *fx.sdg
	ghost := sdg.Target{Callable: dataflow.L3Key{Path: "nowhere.go", Signature: "example.com/nowhere.F"}}
	res.ParamIn = append(slices.Clone(res.ParamIn), sdg.ParamEdge{
		Src: sdg.Ref{Owner: ghost, Key: "1:1/actual_in:0"},
		Dst: sdg.Ref{Owner: ghost, Key: "@formal_in:0"},
		Var: "x",
	})
	var warned []string
	prev := warnf
	warnf = func(format string, args ...any) { warned = append(warned, fmt.Sprintf(format, args...)) }
	defer func() { warnf = prev }()
	app := &schema.GoApplication{SymbolTable: fx.st, Externals: map[string]schema.GoExternalCallee{}}
	out := Emit(app, "dataflow", fx.dir, 4, "t", 3, fx.res, &res)
	for _, e := range out.Application.ParamIn {
		if strings.Contains(e.Src, "nowhere") || strings.Contains(e.Dst, "nowhere") {
			t.Errorf("dangling param_in emitted: %+v", e)
		}
	}
	if len(warned) == 0 || !strings.Contains(strings.Join(warned, "\n"), "nowhere.go") {
		t.Errorf("no warning names the unresolved ref; warned %q", warned)
	}
}

// TestGateL4_PointsToEdges (spec §4.10): AliasPtr's write through the copy
// p reaches the read through req, and req's formal_out, as ddg edges with
// prov ["points-to"], distinct from the reaching-defs edges on the same
// endpoints.
func TestGateL4_PointsToEdges(t *testing.T) {
	fx := buildL4(t, "dataflow")
	mod := v2.ModuleID(fx.l4.Application.ID, "sdg/sdg.go")
	id := v2.CallableID(mod, "example.com/dataflow/sdg.AliasPtr")
	var got [][3]string
	for _, c := range l4Callables(fx.l4) {
		if c.ID != id {
			continue
		}
		for _, e := range c.Ddg {
			if slices.Equal(e.Prov, []string{"points-to"}) {
				got = append(got, [3]string{e.Src, e.Dst, e.Var})
			}
		}
	}
	want := [][3]string{{"360:2", "361:2", "req.UserID"}, {"360:2", "@formal_out:1", "req"}}
	if !slices.Equal(got, want) {
		t.Errorf("AliasPtr points-to ddg = %v, want %v", got, want)
	}
}

// TestSupersetL4_DetectsEveryViolation: the -a 3 ⊆ -a 4 checker is a gate, so
// it must itself fail on each way a superset can break, and accept a pure
// addition. Without this a bug in supersetL4 would make that gate vacuous.
func TestSupersetL4_DetectsEveryViolation(t *testing.T) {
	parse := func(s string) any {
		var v any
		if err := json.Unmarshal([]byte(s), &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	base := `{"application":{"call_graph":[{"src":"a","dst":"b"}],"x":{"f":{"ddg":[{"src":"1","dst":"2","var":"v"}],"k":"v","list":[1,2]}}}}`
	for name, c := range map[string]struct {
		b    string
		want string // a substring of the error; "" means no error
	}{
		"identical":          {base, ""},
		"pure addition":      {`{"application":{"call_graph":[{"src":"a","dst":"b"},{"src":"a","dst":"c"}],"x":{"f":{"ddg":[{"src":"0","dst":"1","var":"w"},{"src":"1","dst":"2","var":"v"}],"k":"v","list":[1,2,3],"new":1}},"extra":true}}`, ""},
		"removed key":        {`{"application":{"call_graph":[{"src":"a","dst":"b"}],"x":{"f":{"ddg":[{"src":"1","dst":"2","var":"v"}],"list":[1,2]}}}}`, "k missing at L4"},
		"changed value":      {`{"application":{"call_graph":[{"src":"a","dst":"b"}],"x":{"f":{"ddg":[{"src":"1","dst":"2","var":"v"}],"k":"w","list":[1,2]}}}}`, "value changed"},
		"shrunk list":        {`{"application":{"call_graph":[{"src":"a","dst":"b"}],"x":{"f":{"ddg":[{"src":"1","dst":"2","var":"v"}],"k":"v","list":[1]}}}}`, "list shrank"},
		"changed type":       {`{"application":{"call_graph":[{"src":"a","dst":"b"}],"x":{"f":{"ddg":[{"src":"1","dst":"2","var":"v"}],"k":"v","list":{"a":1}}}}}`, "list shrank or changed type"},
		"removed ddg edge":   {`{"application":{"call_graph":[{"src":"a","dst":"b"}],"x":{"f":{"ddg":[{"src":"0","dst":"1","var":"w"}],"k":"v","list":[1,2]}}}}`, "missing or changed at L4"},
		"changed call_graph": {`{"application":{"call_graph":[{"src":"a","dst":"z"}],"x":{"f":{"ddg":[{"src":"1","dst":"2","var":"v"}],"k":"v","list":[1,2]}}}}`, "call_graph: edge"},
	} {
		var errs []string
		supersetL4("$", parse(base), parse(c.b), &errs)
		got := strings.Join(errs, "; ")
		switch {
		case c.want == "" && len(errs) > 0:
			t.Errorf("%s: want no error, got %s", name, got)
		case c.want != "" && !strings.Contains(got, c.want):
			t.Errorf("%s: want an error containing %q, got %q", name, c.want, got)
		}
	}
}
