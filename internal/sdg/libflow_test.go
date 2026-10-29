package sdg

import (
	"maps"
	"math/rand"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"golang.org/x/tools/go/ssa"

	"github.com/codellm-devkit/codeanalyzer-go/internal/dataflow"
)

const libflowPkg = "example.com/dataflow/libflow"

func libflowKey(name string) dataflow.L3Key {
	return dataflow.L3Key{Path: filepath.Join("libflow", "libflow.go"), Signature: libflowPkg + "." + name}
}

// TestCHA_InterproceduralLibraryOrigin (round 2): a Store that came out of
// library code reaches an interface call through project code — a project
// function's result, a parameter, a struct field set in another method, a
// global, a closure capture — or through a library container element (map
// lookup, slice and array index, channel receive). Each site's targets
// include sqlStore.Find (the CHA complement), and the caller's footprint
// sees the global sqlStore.Find writes.
func TestCHA_InterproceduralLibraryOrigin(t *testing.T) {
	in, _, d := runDispatch(t)
	find := Target{Callable: libflowKey("sqlStore.Find")}
	fps := footprints(in, d)
	for _, c := range []struct{ fn, site string }{
		{"ViaReturn", "47:9"},
		{"use", "53:9"},
		{"handler.ViaField", "84:9"},
		{"ViaGlobal", "96:9"},
		{"ViaCapture.closure_1", "104:10"},
		{"ViaMap", "112:9"},
		{"ViaSlice", "117:9"},
		{"ViaChan", "122:9"},
		{"ViaArray", "127:9"},
		{"ViaPromoted", "195:9"},
		{"zzzViaPromoted", "205:9"},
	} {
		if _, ok := d.sites[siteKey{libflowKey(c.fn), c.site}]; !ok {
			t.Errorf("%s has no site %s: the keys moved", c.fn, c.site)
			continue
		}
		if got := d.targets(libflowKey(c.fn), c.site); !slices.Contains(got, find) {
			t.Errorf("targets(%s, %s) = %v, want sqlStore.Find among them", c.fn, c.site, got)
		}
		if fp := fps[Target{Callable: libflowKey(c.fn)}]; !slices.Contains(fp.GlobalWrites, libflowPkg+"::Last") {
			t.Errorf("%s writes %v, want %s::Last", c.fn, fp.GlobalWrites, libflowPkg)
		}
	}
}

// TestCHA_ProjectOnlyOriginNoTargets (round 2): ProjectOnly's
// relay(s).Find(id) is a call through a project function's result whose
// every value project code built (a memStore): it gets VTA's memStore.Find
// and the interface method, and no CHA target.
func TestCHA_ProjectOnlyOriginNoTargets(t *testing.T) {
	_, _, d := runDispatch(t)
	want := []Target{{Callable: libflowKey("Store.Find")}, {Callable: libflowKey("memStore.Find")}}
	if got := d.targets(libflowKey("ProjectOnly"), "134:9"); !slices.Equal(got, want) {
		t.Errorf("targets(ProjectOnly, 134:9) = %v, want %v", got, want)
	}
}

// TestCHA_PanicDegradesToNoCHA (N1): a panic inside the CHA complement is
// recovered: every CHA target is dropped (the dispatch is what it would be
// without CHA), no_cha is recorded, and VTA still runs.
func TestCHA_PanicDegradesToNoCHA(t *testing.T) {
	in := loadInput(t, "dataflow")
	defer func() { chaHook = nil }()
	chaHook = func(fn *ssa.Function) {
		if fn.Name() == "ViaMap" {
			panic("cha boom")
		}
	}
	res, d := run(in)
	if codes := codesOf(res.Degraded); !slices.Equal(codes, []string{CodeNoCHA}) {
		t.Fatalf("codes = %v, want [no_cha]", codes)
	}
	// ViaReturn's site keeps only D7 (VTA cannot see the sqlStore).
	if got, want := d.targets(libflowKey("ViaReturn"), "47:9"), []Target{{Callable: libflowKey("Store.Find")}}; !slices.Equal(got, want) {
		t.Errorf("targets(ViaReturn, 47:9) = %v, want %v (no CHA target after no_cha)", got, want)
	}
	// VTA ran: HandleDirect's VTA target is there.
	if got := d.targets(wrapKey("HandleDirect"), "33:9"); !slices.Contains(got, Target{Callable: wrapKey("sqlStore.Find")}) {
		t.Errorf("targets(HandleDirect, 33:9) = %v, want VTA's sqlStore.Find", got)
	}
}

// chaTargets is every CHA run's dispatch over in, with the origin fixpoint
// seeded in the order perm gives (nil: the sorted order), and the LO values
// of every project function, by function and instruction index.
func chaTargets(t *testing.T, in Input, perm func([]*ssa.Function)) (map[siteKey][]Target, map[string]bool) {
	t.Helper()
	prog, pkgs, _ := buildSSA(in)
	funcs, _, err := funcSet(prog, pkgs)
	if err != nil {
		t.Fatal(err)
	}
	j := newJoin(in)
	d := newDispatch(in)
	defer func() { originSeedOrder = nil }()
	originSeedOrder = perm
	d.addCHA(newCHA(j, in), funcs)
	o := newOrigins(newCHA(j, in), funcs)
	originSeedOrder = perm
	o.solve()
	lo := map[string]bool{}
	for _, fn := range o.order {
		vals := o.eval(fn)
		i := 0
		for _, b := range fn.Blocks {
			for _, instr := range b.Instrs {
				if v, ok := instr.(ssa.Value); ok && vals[v] {
					lo[fn.String()+"#"+strconv.Itoa(i)] = true
				}
				i++
			}
		}
		for k, p := range fn.Params {
			if vals[p] {
				lo[fn.String()+"#p"+strconv.Itoa(k)] = true
			}
		}
	}
	return d.sites, lo
}

// TestCHA_OriginOrderIndependent (round 3): the origin fixpoint is the
// least fixpoint, independent of the order functions are visited in. The
// worklist seeded in reverse and in two shuffled orders gives the same LO
// values and the same CHA targets as the sorted order.
func TestCHA_OriginOrderIndependent(t *testing.T) {
	in := loadInput(t, "dataflow")
	wantSites, wantLO := chaTargets(t, in, nil)
	if !wantLO["example.com/dataflow/libflow.zzzGet#p0"] && len(wantLO) == 0 {
		t.Fatal("no LO values: the fixture is vacuous")
	}
	perms := map[string]func([]*ssa.Function){
		"reverse": func(fs []*ssa.Function) { slices.Reverse(fs) },
		"shuffle1": func(fs []*ssa.Function) {
			r := rand.New(rand.NewSource(1))
			r.Shuffle(len(fs), func(i, j int) { fs[i], fs[j] = fs[j], fs[i] })
		},
		"shuffle2": func(fs []*ssa.Function) {
			r := rand.New(rand.NewSource(2))
			r.Shuffle(len(fs), func(i, j int) { fs[i], fs[j] = fs[j], fs[i] })
		},
	}
	for name, perm := range perms {
		sites, lo := chaTargets(t, in, perm)
		if !maps.Equal(lo, wantLO) {
			var diff []string
			for k := range lo {
				if !wantLO[k] {
					diff = append(diff, "+"+k)
				}
			}
			for k := range wantLO {
				if !lo[k] {
					diff = append(diff, "-"+k)
				}
			}
			slices.Sort(diff)
			t.Errorf("%s: LO differs from the sorted order: %v", name, diff)
		}
		if !maps.EqualFunc(sites, wantSites, slices.Equal[[]Target]) {
			for k, ts := range sites {
				if !slices.Equal(ts, wantSites[k]) {
					t.Errorf("%s: site %v targets %v, sorted order %v", name, k, ts, wantSites[k])
				}
			}
		}
	}
}

// TestCHA_PanicRestoresEverySite (round 3): a panic after the CHA
// complement added targets restores every site to exactly what it held
// before CHA ran. d.add inserts in place, so a saved slice that shares its
// backing array with the live one would be shifted under it.
func TestCHA_PanicRestoresEverySite(t *testing.T) {
	in := loadInput(t, "dataflow")
	prog, pkgs, _ := buildSSA(in)
	funcs, _, err := funcSet(prog, pkgs)
	if err != nil {
		t.Fatal(err)
	}
	j := newJoin(in)
	d := newDispatch(in)
	d.addInvokes(j, funcs)
	// Spare capacity in every site, as append-grown slices have.
	for k, ts := range d.sites {
		d.sites[k] = slices.Grow(ts, 8)
	}
	before := map[siteKey][]Target{}
	for k, ts := range d.sites {
		before[k] = slices.Clone(ts)
	}
	defer func() { chaHook = nil }()
	seen := 0
	chaHook = func(*ssa.Function) {
		seen++
		if seen == len(funcs) { // the last function: every earlier site has its CHA targets
			panic("cha boom")
		}
	}
	if err := d.runCHA(j, in, funcs); err == nil {
		t.Fatal("runCHA did not report the panic")
	}
	if len(d.sites) != len(before) {
		t.Errorf("%d sites after the panic, %d before", len(d.sites), len(before))
	}
	for k, ts := range before {
		if got := d.sites[k]; !slices.Equal(got, ts) {
			t.Errorf("site %v = %v after the panic, want %v", k, got, ts)
		}
	}
}
