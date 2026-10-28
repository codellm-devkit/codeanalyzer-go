package sdg

import (
	"path/filepath"
	"slices"
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
