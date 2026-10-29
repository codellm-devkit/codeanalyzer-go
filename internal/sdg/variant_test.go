package sdg

import (
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/codellm-devkit/codeanalyzer-go/internal/dataflow"
)

const variantPkg = "example.com/dataflow/variant/"

var (
	variantUse  = dataflow.L3Key{Path: filepath.Join("variant", "u", "u.go"), Signature: variantPkg + "u.Use"}
	variantImpl = dataflow.L3Key{Path: filepath.Join("variant", "p", "p.go"), Signature: variantPkg + "p.impl.M"}
)

// siteTargetsOf is the union of the dispatch targets of unit's call sites.
func siteTargetsOf(d *dispatch, unit dataflow.L3Key) []Target {
	var out []Target
	for site, ts := range d.sites {
		if site.unit == unit {
			out = append(out, ts...)
		}
	}
	slices.SortFunc(out, compareTarget)
	return slices.Compact(out)
}

// TestSSA_EveryPackageVariant (F2): with tests loaded, go/packages yields
// the test variant "p [p.test]" beside the plain p that u imports. SSA
// must build both: u.Use's p.New().M(x) dispatches by VTA to impl.M only
// when the plain p, whose New u calls, has bodies. Use then writes the
// global impl.M writes, x reaches its <return>, and nothing is degraded.
func TestSSA_EveryPackageVariant(t *testing.T) {
	in := loadInputOpts(t, "dataflow", false)
	res, d := run(in)
	if len(res.Degraded) != 0 {
		t.Fatalf("degraded = %+v, want none", res.Degraded)
	}
	impl := Target{Callable: variantImpl}
	if ts := siteTargetsOf(d, variantUse); !slices.Contains(ts, impl) {
		t.Errorf("Use's targets = %v, want impl.M among them", ts)
	}
	vta := false
	for _, e := range res.CallEdges {
		vta = vta || e.Source == variantUse.Signature && e.Target == variantImpl.Signature && slices.Equal(e.Provenance, []string{"vta"})
	}
	if !vta {
		t.Errorf("no VTA call_graph edge Use -> impl.M: %+v", res.CallEdges)
	}
	fp := footprints(in, d)[Target{Callable: variantUse}]
	if !slices.Contains(fp.GlobalWrites, variantPkg+"p::Last") {
		t.Errorf("Use writes %v, want %sp::Last", fp.GlobalWrites, variantPkg)
	}
	flows := newSDGBuilder(in, d).summarize()
	if !slices.Contains(flows[Target{Callable: variantUse}], [2]int{0, 0}) {
		t.Errorf("Use's flows = %v, want x → <return> ([0 0])", flows[Target{Callable: variantUse}])
	}
	// Every target and param edge appears once though two SSA variants of
	// p join the same callables.
	for site, ts := range d.sites {
		if !slices.IsSortedFunc(ts, compareTarget) || len(slices.CompactFunc(slices.Clone(ts), func(a, b Target) bool { return a == b })) != len(ts) {
			t.Errorf("site %v targets not sorted and unique: %v", site, ts)
		}
	}
	if !slices.IsSortedFunc(res.ParamIn, compareParamEdge) || len(slices.Compact(slices.Clone(res.ParamIn))) != len(res.ParamIn) {
		t.Error("param_in not sorted and unique")
	}
}

// TestSSA_VariantsNeutralWithoutTests (F2): with tests skipped there is one
// variant per path, so the L4 result is the same whether SSA is built from
// in.Variants or from in.Pkgs.
func TestSSA_VariantsNeutralWithoutTests(t *testing.T) {
	in := loadInput(t, "dataflow")
	if len(in.Variants) != len(in.Pkgs) {
		t.Fatalf("%d variants, %d packages: want one variant per path with tests skipped", len(in.Variants), len(in.Pkgs))
	}
	a := Run(in)
	in.Variants = nil
	b := Run(in)
	if !reflect.DeepEqual(a, b) {
		t.Error("L4 result differs between Variants and Pkgs with tests skipped")
	}
}
