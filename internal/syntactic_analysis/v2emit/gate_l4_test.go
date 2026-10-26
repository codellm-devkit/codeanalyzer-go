package v2emit

// L4 conformance harness for the v2 emitter.
//
// buildL4 runs the -a 4 pipeline: the -a 3 one, then sdg.Run, whose VTA
// call_graph edges and VTA-only externals are merged into the application
// before the L4 payload is emitted (as core does). Later tasks fill in more L4
// content, and the monotonicity gate below keeps holding as they do
// (-a 3 ⊆ -a 4).

import (
	"encoding/json"
	"path/filepath"
	"runtime"
	"slices"
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
}

var (
	l4Mu    sync.Mutex
	l4Cache = map[string]*l4Fixture{}
)

// buildL4 runs the -a 4 pipeline over testdata/<name> once and emits both the
// L3 and the L4 payload from the same resolved symbol table.
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
	fx := &l4Fixture{st: st, l3: Emit(app, name, dir, 3, "gate-test", 3, res), res: res}
	l4 := sdg.Run(sdg.Input{
		Fset: b.Fset(), Pkgs: b.Pkgs(), ProjectDir: dir, SymbolTable: st,
		Externals: app.Externals, CallGraph: app.CallGraph,
		Units: units, L3: res, K: 3, Jobs: 4,
	})
	l4.ApplyTo(app)
	fx.l4 = Emit(app, name, dir, 4, "gate-test", 3, res)
	l4Cache[name] = fx
	return fx
}

// supersetL4 records where a (-a 3) is not contained in b (-a 4): every object
// key and value, and every list entry, by position — except call_graph, a
// sorted edge list that VTA adds edges into, where every -a 3 edge must occur
// unchanged somewhere in the -a 4 list.
func supersetL4(path string, a, b any, errs *[]string) {
	if path == "$.application.call_graph" {
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
	for _, name := range []string{"dataflow", "chi", "generics"} {
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
