package sdg

import (
	"math/rand"

	"golang.org/x/tools/go/callgraph"
	"golang.org/x/tools/go/ssa"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/codellm-devkit/codeanalyzer-go/internal/dataflow"
	"github.com/codellm-devkit/codeanalyzer-go/internal/schema"
)

const configSpell = sdgPkg + "::Config"

// globalsKey is the L3Key of a callable declared in
// testdata/dataflow/sdg/globals.go.
func globalsKey(name string) dataflow.L3Key {
	return dataflow.L3Key{Path: filepath.Join("sdg", "globals.go"), Signature: sdgPkg + "." + name}
}

func runFootprints(t *testing.T) (Input, *dispatch, map[Target]Footprint) {
	t.Helper()
	in, _, d := runDispatch(t)
	return in, d, footprints(in, d)
}

func fpOf(t *testing.T, fps map[Target]Footprint, tg Target) Footprint {
	t.Helper()
	fp, ok := fps[tg]
	if !ok {
		t.Fatalf("no footprint for %v", tg)
	}
	return fp
}

// TestFootprint_TransitiveGlobalReads: useConfig never names Config, but it
// calls readConfig, which reads it; initConfig writes it.
func TestFootprint_TransitiveGlobalReads(t *testing.T) {
	_, _, fps := runFootprints(t)
	want := []string{configSpell}
	if got := fpOf(t, fps, sdgTarget("useConfig")); !slices.Equal(got.GlobalReads, want) || len(got.GlobalWrites) != 0 {
		t.Errorf("useConfig = %+v, want reads %v, no writes", got, want)
	}
	if got := fpOf(t, fps, sdgTarget("initConfig")); !slices.Equal(got.GlobalWrites, want) || len(got.GlobalReads) != 0 {
		t.Errorf("initConfig = %+v, want writes %v, no reads", got, want)
	}
}

// TestFootprint_GlobalFilter (A5): constOnly (literal initializer, never
// written) is dropped everywhere; os::Args (a library variable) is kept, and
// so are home (its initializer calls os.Getenv) and counter (written only
// through &counter).
func TestFootprint_GlobalFilter(t *testing.T) {
	_, _, fps := runFootprints(t)
	for tg, fp := range fps {
		for _, g := range slices.Concat(fp.GlobalReads, fp.GlobalWrites) {
			if strings.HasSuffix(g, "::constOnly") {
				t.Errorf("%v has filtered global %s", tg, g)
			}
		}
	}
	if got := fpOf(t, fps, sdgTarget("readConst")); len(got.GlobalReads) != 0 {
		t.Errorf("readConst reads %v, want none", got.GlobalReads)
	}
	for _, c := range []struct {
		fn   string
		want string
	}{
		{"readArgs", "os::Args"},
		{"readHome", sdgPkg + "::home"},
		{"readCounter", sdgPkg + "::counter"},
	} {
		if got := fpOf(t, fps, Target{Callable: globalsKey(c.fn)}); !slices.Equal(got.GlobalReads, []string{c.want}) {
			t.Errorf("%s reads %v, want [%s]", c.fn, got.GlobalReads, c.want)
		}
	}
}

// TestFootprint_MutualRecursionFixpoint: readConfig and useConfig are one
// SCC and agree on its footprint. On a synthetic graph where the fact sits on
// the SCC member sorted last, only a fixpoint gives it to the first one (and
// to a caller of the SCC).
func TestFootprint_MutualRecursionFixpoint(t *testing.T) {
	in, d, fps := runFootprints(t)
	nodes, callees := callGraph(in, d)
	comps := sccs(nodes, func(tg Target) []Target { return callees[tg] })
	want := []Target{sdgTarget("readConfig"), sdgTarget("useConfig")}
	if !slices.ContainsFunc(comps, func(c []Target) bool { return slices.Equal(c, want) }) {
		t.Errorf("no SCC %v", want)
	}
	for _, tg := range want {
		if got := fpOf(t, fps, tg); !slices.Equal(got.GlobalReads, []string{configSpell}) {
			t.Errorf("%v reads %v, want [%s]", tg, got.GlobalReads, configSpell)
		}
	}

	a, b, c := sdgTarget("a"), sdgTarget("b"), sdgTarget("c")
	syn := map[Target][]Target{a: {b}, b: {a}, c: {a}}
	direct := map[Target]globals{b: {reads: []string{"p::G"}, writes: []string{"p::H"}}}
	got := propagate([]Target{c, b, a}, syn, direct)
	for _, tg := range []Target{a, b, c} {
		if g := got[tg]; !slices.Equal(g.reads, []string{"p::G"}) || !slices.Equal(g.writes, []string{"p::H"}) {
			t.Errorf("%v = %+v, want reads [p::G], writes [p::H]", tg, g)
		}
	}
}

// TestFootprint_Mutated (A10, fix I1): a callable mutates a by-ref param or
// receiver (the shared predicate) it uses or writes; a stand-in, an interface
// method and a bodiless declaration mutate every by-ref formal.
func TestFootprint_Mutated(t *testing.T) {
	_, _, fps := runFootprints(t)
	for _, c := range []struct {
		tg   Target
		want []string
	}{
		{sdgTarget("mutate"), []string{"p", "q", "s"}}, // n is a copy holding no reference; q is used (`_ = q`)
		{sdgTarget("sqlStore.setDSN"), []string{"s"}},
		{sdgTarget("memStore.Get"), []string{"s"}},  // s holds a map and is used: a use is enough (I1)
		{sdgTarget("Store.Get"), []string{"_recv"}}, // interface method: the receiver is an interface
		{Target{External: "io.Writer.Write"}, []string{"_recv", "p"}},
		{Target{External: "bytes.Buffer.Write"}, []string{"b", "p"}},
		{Target{Callable: dataflow.L3Key{Path: filepath.Join("asm", "asm.go"), Signature: "example.com/dataflow/asm.Add"}}, nil},
	} {
		if got := fpOf(t, fps, c.tg); !slices.Equal(got.Mutated, c.want) {
			t.Errorf("%v mutates %v, want %v", c.tg, got.Mutated, c.want)
		}
	}
}

// TestFootprint_L3SkippedMutatesEveryByRefFormal: with its L3 result gone,
// mutate has no direct globals and mutates every by-ref formal (p, s and q;
// n holds no reference).
func TestFootprint_L3SkippedMutatesEveryByRefFormal(t *testing.T) {
	in, _, d := runDispatch(t)
	l3 := make(map[dataflow.L3Key]dataflow.Result, len(in.L3))
	for k, r := range in.L3 {
		if k != sdgKey("mutate") {
			l3[k] = r
		}
	}
	in.L3 = l3
	got := fpOf(t, footprints(in, d), sdgTarget("mutate"))
	if want := []string{"p", "q", "s"}; !slices.Equal(got.Mutated, want) || len(got.GlobalReads)+len(got.GlobalWrites) != 0 {
		t.Errorf("l3-skipped mutate = %+v, want mutated %v, no globals", got, want)
	}
}

// TestSDG_GenericInstancesShareOneCallable (Review Focus 2): the join maps a
// generic instance to its origin, so testdata/dataflow/generic has one
// footprint per origin callable and none per instance.
func TestSDG_GenericInstancesShareOneCallable(t *testing.T) {
	in, _, fps := runFootprints(t)
	units := map[dataflow.L3Key]bool{}
	for _, u := range in.Units {
		if strings.HasPrefix(u.Key.Path, "generic"+string(filepath.Separator)) {
			units[u.Key] = true
		}
	}
	got := map[dataflow.L3Key]bool{}
	for tg := range fps {
		if strings.HasPrefix(tg.Callable.Path, "generic"+string(filepath.Separator)) {
			got[tg.Callable] = true
		}
	}
	if len(units) == 0 || len(got) != len(units) {
		t.Fatalf("generic footprint keys = %v, want the %d units %v", got, len(units), units)
	}
	for k := range got {
		if !units[k] || strings.Contains(k.Signature, "[") {
			t.Errorf("footprint key %v is not an origin unit", k)
		}
	}
	twice := dataflow.L3Key{Path: filepath.Join("generic", "b.go"), Signature: "example.com/dataflow/generic.Twice"}
	if !units[twice] {
		t.Fatalf("no unit %v", twice)
	}
	storeA := fpOf(t, fps, Target{Callable: dataflow.L3Key{Path: filepath.Join("generic", "a.go"), Signature: "example.com/dataflow/generic.StoreA"}})
	if want := []string{"example.com/dataflow/generic::pa"}; !slices.Equal(storeA.GlobalWrites, want) {
		t.Errorf("StoreA writes %v, want %v (pa.Store has a pointer receiver)", storeA.GlobalWrites, want)
	}
}

// TestSCC_Deterministic: shuffling the node order and every successor list
// gives the same SCCs, callees first, each sorted.
func TestSCC_Deterministic(t *testing.T) {
	n := func(s string) Target { return Target{External: s} }
	edges := map[Target][]Target{
		n("a"): {n("b")},
		n("b"): {n("c"), n("e")},
		n("c"): {n("a"), n("d")},
		n("d"): {},
		n("e"): {n("f")},
		n("f"): {n("e"), n("g")},
		n("g"): {n("g")},
		n("h"): {n("a"), n("x")}, // x is reachable only: not in nodes
	}
	var nodes []Target
	for k := range edges {
		nodes = append(nodes, k)
	}
	slices.SortFunc(nodes, compareTarget)
	want := sccs(nodes, func(tg Target) []Target { return edges[tg] })
	wantStr := [][]Target{{n("d")}, {n("g")}, {n("e"), n("f")}, {n("a"), n("b"), n("c")}, {n("x")}, {n("h")}}
	if !slices.EqualFunc(want, wantStr, slices.Equal) {
		t.Fatalf("sccs = %v, want %v", want, wantStr)
	}
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 50; i++ {
		shuf := slices.Clone(nodes)
		rng.Shuffle(len(shuf), func(i, j int) { shuf[i], shuf[j] = shuf[j], shuf[i] })
		se := map[Target][]Target{}
		for k, v := range edges {
			v = slices.Clone(v)
			rng.Shuffle(len(v), func(i, j int) { v[i], v[j] = v[j], v[i] })
			se[k] = v
		}
		if got := sccs(shuf, func(tg Target) []Target { return se[tg] }); !slices.EqualFunc(got, want, slices.Equal) {
			t.Fatalf("shuffle %d: sccs = %v, want %v", i, got, want)
		}
	}
}

// TestFootprint_MutatedThroughAlias (fix I1): a by-ref formal used at any
// node other than @entry is mutated — a write through a local alias (a range
// variable, q := p) or passing the alias on is not a def of the formal at L3.
// One never used is not.
func TestFootprint_MutatedThroughAlias(t *testing.T) {
	_, _, fps := runFootprints(t)
	for _, c := range []struct {
		fn   string
		want []string
	}{
		{"aliasRange", []string{"s"}},
		{"aliasAssign", []string{"p"}},
		{"aliasCall", []string{"p"}},
		{"unusedRef", nil},
	} {
		if got := fpOf(t, fps, sdgTarget(c.fn)); !slices.Equal(got.Mutated, c.want) {
			t.Errorf("%s mutates %v, want %v", c.fn, got.Mutated, c.want)
		}
	}
}

// TestFootprint_L3SkippedKeepsGlobalsAndCallees (fix I2): with skipX's L3
// result gone, skipX still has the global it names (g1, as read and written)
// and its callee skipY's (g2), and so does its caller.
func TestFootprint_L3SkippedKeepsGlobalsAndCallees(t *testing.T) {
	in, _, d := runDispatch(t)
	skipped := globalsKey("skipX")
	l3 := make(map[dataflow.L3Key]dataflow.Result, len(in.L3))
	for k, r := range in.L3 {
		if k != skipped {
			l3[k] = r
		}
	}
	in.L3 = l3
	fps := footprints(in, d)
	g1, g2 := sdgPkg+"::g1", sdgPkg+"::g2"
	for _, k := range []dataflow.L3Key{skipped, globalsKey("callsSkipX")} {
		got := fpOf(t, fps, Target{Callable: k})
		if !slices.Equal(got.GlobalReads, []string{g1, g2}) || !slices.Contains(got.GlobalWrites, g1) {
			t.Errorf("%s = %+v, want reads [%s %s] and writes ⊇ [%s]", k.Signature, got, g1, g2, g1)
		}
	}

	// With no dispatch sites either (a call-count mismatch leaves
	// newDispatch nothing for it), the L2 call graph still gives its callees.
	for site := range d.sites {
		if site.unit == skipped {
			delete(d.sites, site)
		}
	}
	got := fpOf(t, footprints(in, d), Target{Callable: globalsKey("callsSkipX")})
	if !slices.Contains(got.GlobalReads, g2) {
		t.Errorf("callsSkipX reads %v without skipX's dispatch sites, want ⊇ [%s]", got.GlobalReads, g2)
	}
}

// TestFootprint_LiteralPassedToLibrary (fix I3): Get hands a literal to
// once.Do, which never calls it, so no call edge reaches the literal; the
// encloser → literal edge still gives Get and its caller the literal's write
// of inst.
func TestFootprint_LiteralPassedToLibrary(t *testing.T) {
	_, _, fps := runFootprints(t)
	inst := sdgPkg + "::inst"
	for _, fn := range []string{"Get", "callsGet"} {
		if got := fpOf(t, fps, Target{Callable: globalsKey(fn)}); !slices.Contains(got.GlobalWrites, inst) {
			t.Errorf("%s writes %v, want ⊇ [%s]", fn, got.GlobalWrites, inst)
		}
	}
}

// TestFootprint_ChainedInitializers (fix M4): a global initialized from a
// library variable (argv = os.Args) or from a kept global (cfgDir = home +
// "/x") is kept.
func TestFootprint_ChainedInitializers(t *testing.T) {
	_, _, fps := runFootprints(t)
	for _, c := range []struct{ fn, want string }{
		{"readArgv", sdgPkg + "::argv"},
		{"readCfgDir", sdgPkg + "::cfgDir"},
	} {
		if got := fpOf(t, fps, Target{Callable: globalsKey(c.fn)}); !slices.Equal(got.GlobalReads, []string{c.want}) {
			t.Errorf("%s reads %v, want [%s]", c.fn, got.GlobalReads, c.want)
		}
	}
}

// TestFootprint_CalledLiteralsWithoutVTA (fix N1): with VTA failed, only VTA
// would give a call edge to a literal called in place (L2 resolves no
// FuncLit callee), so the encloser → literal edge must cover every literal:
// calledLits writes litG (`func(){…}()`) and litH (`defer func(){…}()`).
func TestFootprint_CalledLiteralsWithoutVTA(t *testing.T) {
	in := loadInput(t, "dataflow")
	defer func(f func(map[*ssa.Function]bool, *callgraph.Graph) *callgraph.Graph) { vtaCallGraph = f }(vtaCallGraph)
	vtaCallGraph = func(map[*ssa.Function]bool, *callgraph.Graph) *callgraph.Graph { panic("vta boom") }
	_, d := run(in)
	got := fpOf(t, footprints(in, d), Target{Callable: globalsKey("calledLits")})
	if want := []string{sdgPkg + "::litG", sdgPkg + "::litH"}; !slices.Equal(got.GlobalWrites, want) {
		t.Errorf("calledLits writes %v under no_vta, want %v", got.GlobalWrites, want)
	}
}

// TestFootprint_L2FallbackHonoursTargetPath (fix N2): an L2 edge with a
// TargetPath maps to that file's callable, not to the lowest-path twin of a
// repeated signature (both files' init closures are
// initclosure.init.closure_1). The edge is synthetic, from an L3-skipped
// unit with no dispatch sites.
func TestFootprint_L2FallbackHonoursTargetPath(t *testing.T) {
	in, _, d := runDispatch(t)
	const pkg = "example.com/dataflow/initclosure"
	src := globalsKey("callsSkipX")
	b := dataflow.L3Key{Path: filepath.Join("initclosure", "b.go"), Signature: pkg + ".init.closure_1"}
	l3 := make(map[dataflow.L3Key]dataflow.Result, len(in.L3))
	for k, r := range in.L3 {
		if k != src {
			l3[k] = r
		}
	}
	in.L3 = l3
	for site := range d.sites {
		if site.unit == src {
			delete(d.sites, site)
		}
	}
	in.CallGraph = []schema.GoCallEdge{{Source: src.Signature, SourcePath: src.Path, Target: b.Signature, TargetPath: b.Path}}
	_, callees := callGraph(in, d)
	if got, want := callees[Target{Callable: src}], []Target{{Callable: b}}; !slices.Equal(got, want) {
		t.Errorf("callees(%s) = %v, want %v", src.Signature, got, want)
	}
}
