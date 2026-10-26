package sdg

import (
	"math/rand"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/codellm-devkit/codeanalyzer-go/internal/dataflow"
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

// TestFootprint_Mutated (A10): a callable mutates a param or receiver it
// writes through, by the shared by-ref predicate; a stand-in, an interface
// method and a bodiless declaration mutate every by-ref formal.
func TestFootprint_Mutated(t *testing.T) {
	_, _, fps := runFootprints(t)
	for _, c := range []struct {
		tg   Target
		want []string
	}{
		{sdgTarget("mutate"), []string{"p", "s"}}, // n is a copy holding no reference; q is never written
		{sdgTarget("sqlStore.setDSN"), []string{"s"}},
		{sdgTarget("memStore.Get"), nil},            // reads through s only
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
