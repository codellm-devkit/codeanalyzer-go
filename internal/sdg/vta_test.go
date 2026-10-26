package sdg

import (
	"cmp"
	"path/filepath"
	"slices"
	"testing"

	"golang.org/x/tools/go/callgraph"
	"golang.org/x/tools/go/ssa"

	"github.com/codellm-devkit/codeanalyzer-go/internal/dataflow"
	"github.com/codellm-devkit/codeanalyzer-go/internal/schema"
)

const sdgPkg = "example.com/dataflow/sdg"

var sdgFile = filepath.Join("sdg", "sdg.go")

// sdgKey is the L3Key of a callable declared in testdata/dataflow/sdg/sdg.go.
func sdgKey(name string) dataflow.L3Key {
	return dataflow.L3Key{Path: sdgFile, Signature: sdgPkg + "." + name}
}

func sdgTarget(name string) Target { return Target{Callable: sdgKey(name)} }

// runDispatch runs L4 over testdata/dataflow and returns its result and the
// call-site dispatch.
func runDispatch(t *testing.T) (Input, Result, *dispatch) {
	t.Helper()
	in := loadInput(t, "dataflow")
	res, d := run(in)
	if len(res.Degraded) != 0 {
		t.Fatalf("degraded = %+v, want none", res.Degraded)
	}
	return in, res, d
}

// hasEdge reports whether edges holds src → dst (src declared in sdg.go)
// with prov ["vta"] and weight 1.
func hasVTAEdge(edges []schema.GoCallEdge, src, dst string) bool {
	for _, e := range edges {
		if e.Source == src && e.Target == dst && e.SourcePath == sdgFile {
			return slices.Equal(e.Provenance, []string{"vta"}) && e.Weight == 1
		}
	}
	return false
}

func hasEdgeTo(edges []schema.GoCallEdge, src, dst string) bool {
	for _, e := range edges {
		if e.Source == src && e.Target == dst {
			return true
		}
	}
	return false
}

// TestVTA_InterfaceTargets: UseStore's s.Get(k) dispatches to the interface
// method (D7) and to both implementations; memStore's pointer wrapper (from
// UseStore(&memStore{})) maps to memStore.Get. Only the concrete targets are
// new call_graph edges: L2 already has UseStore → Store.Get.
func TestVTA_InterfaceTargets(t *testing.T) {
	_, res, d := runDispatch(t)
	got := d.targets(sdgKey("UseStore"), "34:9")
	want := []Target{sdgTarget("Store.Get"), sdgTarget("memStore.Get"), sdgTarget("sqlStore.Get")}
	if !slices.Equal(got, want) {
		t.Fatalf("targets(UseStore, 34:9) = %v, want %v", got, want)
	}
	src := sdgPkg + ".UseStore"
	for _, dst := range []string{sdgPkg + ".memStore.Get", sdgPkg + ".sqlStore.Get"} {
		if !hasVTAEdge(res.CallEdges, src, dst) {
			t.Errorf("CallEdges lack %s → %s (prov [vta], weight 1): %+v", src, dst, res.CallEdges)
		}
	}
	if hasEdgeTo(res.CallEdges, src, sdgPkg+".Store.Get") {
		t.Errorf("CallEdges repeat the L2 edge %s → Store.Get", src)
	}
}

// TestVTA_FuncValueTargets: CallFuncValue's f(1) has no static callee; VTA
// resolves it to double, the function PassDouble passes.
func TestVTA_FuncValueTargets(t *testing.T) {
	_, res, d := runDispatch(t)
	got := d.targets(sdgKey("CallFuncValue"), "51:9")
	if want := []Target{sdgTarget("double")}; !slices.Equal(got, want) {
		t.Fatalf("targets(CallFuncValue, 51:9) = %v, want %v", got, want)
	}
	if !hasVTAEdge(res.CallEdges, sdgPkg+".CallFuncValue", sdgPkg+".double") {
		t.Errorf("CallEdges lack CallFuncValue → double: %+v", res.CallEdges)
	}
	// A static call keeps its one L2 target.
	got = d.targets(sdgKey("PassDouble"), "56:9")
	if want := []Target{sdgTarget("CallFuncValue")}; !slices.Equal(got, want) {
		t.Errorf("targets(PassDouble, 56:9) = %v, want %v", got, want)
	}
}

// TestVTA_PromotedAndWrapperTargetsMap (Review Focus 3): VTA returns the
// synthetic wrappers — Outer's promotion wrapper and *Outer's
// promotion-and-indirection wrapper for o.M(), *memStore's pointer wrapper for
// s.Get(k) — and each maps to the declared method, not to nothing.
func TestVTA_PromotedAndWrapperTargetsMap(t *testing.T) {
	in := loadInput(t, "dataflow")
	prog, pkgs, _ := buildSSA(in)
	_, all, err := funcSet(prog, pkgs)
	if err != nil {
		t.Fatal(err)
	}
	cg, err := runVTA(all)
	if err != nil {
		t.Fatal(err)
	}
	j := newJoin(in)
	raw := func(unit dataflow.L3Key, key string) []*ssa.Function {
		var out []*ssa.Function
		for _, node := range cg.Nodes {
			for _, e := range node.Out {
				if u, k, ok := j.callSite(e.Site); ok && u == unit && k == key {
					out = append(out, e.Callee.Func)
				}
			}
		}
		return out
	}
	for _, c := range []struct {
		unit     dataflow.L3Key
		key      string
		wrappers int
		want     dataflow.L3Key
	}{
		{sdgKey("CallPromoted"), "80:9", 2, sdgKey("Inner.M")},
		{sdgKey("UseStore"), "34:9", 1, sdgKey("memStore.Get")},
	} {
		n := 0
		for _, fn := range raw(c.unit, c.key) {
			if fn.Synthetic == "" {
				continue
			}
			n++
			if k, ok := j.fnUnit(fn); !ok || k != c.want {
				t.Errorf("wrapper %s [%s] maps to %v, %v; want %v", fn, fn.Synthetic, k, ok, c.want)
			}
		}
		if n != c.wrappers {
			t.Errorf("%v %s: %d wrapper callees, want %d (callees %v)", c.unit, c.key, n, c.wrappers, raw(c.unit, c.key))
		}
	}

	_, res, d := runDispatch(t)
	got := d.targets(sdgKey("CallPromoted"), "80:9")
	if want := []Target{sdgTarget("Inner.M"), sdgTarget("Mer.M")}; !slices.Equal(got, want) {
		t.Fatalf("targets(CallPromoted, 80:9) = %v, want %v", got, want)
	}
	if !hasVTAEdge(res.CallEdges, sdgPkg+".CallPromoted", sdgPkg+".Inner.M") {
		t.Errorf("CallEdges lack CallPromoted → Inner.M: %+v", res.CallEdges)
	}
}

// TestVTA_LibraryTargetBecomesExternal (A19): Buffered's w.Write(p) reaches
// (*bytes.Buffer).Write only through VTA. The target is the external
// bytes.Buffer.Write, recorded in NewExternals with its receiver, beside the
// interface method io.Writer.Write (D7), which L2 already recorded.
func TestVTA_LibraryTargetBecomesExternal(t *testing.T) {
	in, res, d := runDispatch(t)
	got := d.targets(sdgKey("Buffered"), "93:2")
	if want := []Target{{External: "bytes.Buffer.Write"}, {External: "io.Writer.Write"}}; !slices.Equal(got, want) {
		t.Fatalf("targets(Buffered, 93:2) = %v, want %v", got, want)
	}
	if _, ok := in.Externals["bytes.Buffer.Write"]; ok {
		t.Fatal("L2 already records bytes.Buffer.Write; the fixture no longer pins a VTA-only external")
	}
	if _, ok := in.Externals["io.Writer.Write"]; !ok {
		t.Fatal("L2 does not record io.Writer.Write")
	}
	ext, ok := res.NewExternals["bytes.Buffer.Write"]
	if !ok {
		t.Fatalf("NewExternals lack bytes.Buffer.Write: %v", res.NewExternals)
	}
	if ext.ImportPath != "bytes" || ext.Sig != "Buffer.Write" || ext.Name != "Write" || ext.ReturnType != "(int, error)" {
		t.Errorf("bytes.Buffer.Write = %+v", ext)
	}
	if len(ext.Params) != 1 || ext.Params[0].Name != "p" || ext.Params[0].Type != "[]byte" {
		t.Errorf("bytes.Buffer.Write params = %+v, want [p []byte]", ext.Params)
	}
	if ext.Receiver == nil || ext.Receiver.Name != "b" || ext.Receiver.Type != "*bytes.Buffer" {
		t.Errorf("bytes.Buffer.Write receiver = %+v, want b *bytes.Buffer", ext.Receiver)
	}
	if _, ok := res.NewExternals["io.Writer.Write"]; ok {
		t.Error("NewExternals repeat the L2 external io.Writer.Write")
	}
	for sig := range res.NewExternals {
		if _, ok := in.Externals[sig]; ok {
			t.Errorf("NewExternals repeat the L2 external %s", sig)
		}
	}
	if !hasVTAEdge(res.CallEdges, sdgPkg+".Buffered", "bytes.Buffer.Write") {
		t.Errorf("CallEdges lack Buffered → bytes.Buffer.Write: %+v", res.CallEdges)
	}
}

// TestVTA_EdgesNewSortedAndResolvable: every VTA edge is new (not an L2
// (caller, callee) pair), the list is sorted and duplicate-free, its source
// is a unit, and its target is a symbol-table callable or an external
// (L2's or a new one).
func TestVTA_EdgesNewSortedAndResolvable(t *testing.T) {
	in, res, _ := runDispatch(t)
	if len(res.CallEdges) == 0 {
		t.Fatal("no VTA edges")
	}
	type key struct{ path, src, dst, dstPath string }
	l2 := map[key]bool{}
	for _, e := range in.CallGraph {
		l2[key{e.SourcePath, e.Source, e.Target, ""}] = true
	}
	units := map[dataflow.L3Key]bool{}
	for _, u := range in.Units {
		units[u.Key] = true
	}
	sigs := map[string]bool{} // every callable the emitter indexes, closures included
	var walk func(c schema.GoCallable)
	walk = func(c schema.GoCallable) {
		sigs[c.Signature] = true
		for _, ic := range c.InnerCallables {
			walk(ic)
		}
	}
	for _, f := range in.SymbolTable {
		for _, c := range f.Functions {
			walk(c)
		}
		for _, ty := range f.Types {
			for _, c := range ty.Methods {
				walk(c)
			}
		}
	}
	for i, e := range res.CallEdges {
		k := key{e.SourcePath, e.Source, e.Target, e.TargetPath}
		if l2[key{k.path, k.src, k.dst, ""}] {
			t.Errorf("edge %+v is already in L2", e)
		}
		if i > 0 {
			p := res.CallEdges[i-1]
			pk := key{p.SourcePath, p.Source, p.Target, p.TargetPath}
			if c := cmp.Or(cmp.Compare(pk.path, k.path), cmp.Compare(pk.src, k.src), cmp.Compare(pk.dst, k.dst), cmp.Compare(pk.dstPath, k.dstPath)); c >= 0 {
				t.Errorf("edges %+v, %+v out of order or duplicated", p, e)
			}
		}
		if !units[dataflow.L3Key{Path: e.SourcePath, Signature: e.Source}] {
			t.Errorf("edge source %s (%s) is not a unit", e.Source, e.SourcePath)
		}
		_, l2ext := in.Externals[e.Target]
		_, newExt := res.NewExternals[e.Target]
		if !sigs[e.Target] && !l2ext && !newExt {
			t.Errorf("edge target %s resolves to nothing", e.Target)
		}
	}
}

// TestVTA_Deterministic: two runs give the same targets, edges and externals.
func TestVTA_Deterministic(t *testing.T) {
	_, a, da := runDispatch(t)
	_, b, db := runDispatch(t)
	if !slices.EqualFunc(a.CallEdges, b.CallEdges, func(x, y schema.GoCallEdge) bool {
		return x.Source == y.Source && x.Target == y.Target && x.SourcePath == y.SourcePath && x.TargetPath == y.TargetPath &&
			slices.Equal(x.Provenance, y.Provenance) && x.Weight == y.Weight
	}) {
		t.Error("CallEdges differ between runs")
	}
	if len(da.sites) != len(db.sites) {
		t.Fatalf("%d vs %d sites", len(da.sites), len(db.sites))
	}
	for s, ts := range da.sites {
		if !slices.Equal(ts, db.sites[s]) {
			t.Errorf("site %v: %v vs %v", s, ts, db.sites[s])
		}
	}
	if len(a.NewExternals) != len(b.NewExternals) {
		t.Errorf("%d vs %d new externals", len(a.NewExternals), len(b.NewExternals))
	}
}

// TestVTA_NoVTAKeepsStaticTargets: when VTA fails, every site keeps its L2
// target (the interface method for an interface call) and no edge is added.
func TestVTA_NoVTAKeepsStaticTargets(t *testing.T) {
	in := loadInput(t, "dataflow")
	defer func(f func(map[*ssa.Function]bool, *callgraph.Graph) *callgraph.Graph) { vtaCallGraph = f }(vtaCallGraph)
	vtaCallGraph = func(map[*ssa.Function]bool, *callgraph.Graph) *callgraph.Graph { panic("vta boom") }
	res, d := run(in)
	if len(res.CallEdges) != 0 || len(res.NewExternals) != 0 {
		t.Errorf("no_vta run added %d edges, %d externals", len(res.CallEdges), len(res.NewExternals))
	}
	if got, want := d.targets(sdgKey("UseStore"), "34:9"), []Target{sdgTarget("Store.Get")}; !slices.Equal(got, want) {
		t.Errorf("targets(UseStore, 34:9) = %v, want %v", got, want)
	}
	if got, want := d.targets(sdgKey("Buffered"), "93:2"), []Target{{External: "io.Writer.Write"}}; !slices.Equal(got, want) {
		t.Errorf("targets(Buffered, 93:2) = %v, want %v", got, want)
	}
	if got := d.targets(sdgKey("CallFuncValue"), "51:9"); len(got) != 0 {
		t.Errorf("targets(CallFuncValue, 51:9) = %v, want none", got)
	}
}

// TestVTA_InitClosureTargetsKeepTheirFile: Run's h() reaches the closure of
// each file's init. Both share the signature initclosure.init.closure_1, so
// each target, and each edge's TargetPath, names its own declaring file.
func TestVTA_InitClosureTargetsKeepTheirFile(t *testing.T) {
	_, res, d := runDispatch(t)
	const pkg = "example.com/dataflow/initclosure"
	a, b := filepath.Join("initclosure", "a.go"), filepath.Join("initclosure", "b.go")
	sig := pkg + ".init.closure_1"
	got := d.targets(dataflow.L3Key{Path: a, Signature: pkg + ".Run"}, "16:8")
	want := []Target{{Callable: dataflow.L3Key{Path: a, Signature: sig}}, {Callable: dataflow.L3Key{Path: b, Signature: sig}}}
	if !slices.Equal(got, want) {
		t.Fatalf("targets(Run, 16:8) = %v, want %v", got, want)
	}
	files := map[string]bool{}
	for _, e := range res.CallEdges {
		if e.Source == pkg+".Run" && e.Target == sig {
			files[e.TargetPath] = true
		}
	}
	if !files[a] || !files[b] || len(files) != 2 {
		t.Errorf("Run → %s edges have TargetPaths %v, want %s and %s", sig, files, a, b)
	}
}
