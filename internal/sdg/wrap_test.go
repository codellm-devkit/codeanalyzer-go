package sdg

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/codellm-devkit/codeanalyzer-go/internal/dataflow"
)

const wrapPkg = "example.com/dataflow/wrap"

// wrapKey is the L3Key of a callable declared in testdata/dataflow/wrap/wrap.go.
func wrapKey(name string) dataflow.L3Key {
	return dataflow.L3Key{Path: filepath.Join("wrap", "wrap.go"), Signature: wrapPkg + "." + name}
}

// TestVTA_TargetsBehindSyntheticWrappers (F1): VTA returns a synthetic
// wrapper for HandleBound's f(id) ((Store).Find$bound) and HandleDecorated's
// l.Find(id) (logged's promotion wrapper); the wrapper's own call of Find is
// a synthetic call site no call node joins. The wrapper's VTA targets are the
// original site's: each handler's site reaches sqlStore.Find, as the direct
// interface call does, and so does the global sqlStore.Find writes.
func TestVTA_TargetsBehindSyntheticWrappers(t *testing.T) {
	in, _, d := runDispatch(t)
	find := Target{Callable: wrapKey("sqlStore.Find")}
	fps := footprints(in, d)
	const global = wrapPkg + "::lastQuery"
	for _, c := range []struct{ fn, site string }{
		{"HandleDirect", "33:9"},
		{"HandleBound", "39:9"},
		{"HandleDecorated", "50:9"},
	} {
		got := d.targets(wrapKey(c.fn), c.site)
		if !slices.Contains(got, find) {
			t.Errorf("targets(%s, %s) = %v, want sqlStore.Find among them", c.fn, c.site, got)
		}
		fp := fps[Target{Callable: wrapKey(c.fn)}]
		if !slices.Contains(fp.GlobalWrites, global) {
			t.Errorf("%s footprint writes %v, want %s", c.fn, fp.GlobalWrites, global)
		}
	}
}

// TestVTA_NoEdgeForTargetsVTADidNotFind (F7): a call_graph edge prov ["vta"]
// is one VTA produced. HandleBound's f(id) has no L2 callee; its targets
// include the interface method Store.Find (bound by D7, from the $bound
// wrapper VTA returned, not a VTA callee of the site's own) and sqlStore.Find
// (VTA, through the wrapper). Only the latter is an edge. The interface
// method stays a target: its param edges are unchanged.
func TestVTA_NoEdgeForTargetsVTADidNotFind(t *testing.T) {
	_, res, d := runDispatch(t)
	src := wrapKey("HandleBound")
	if ts := d.targets(src, "39:9"); !slices.Contains(ts, Target{Callable: wrapKey("Store.Find")}) {
		t.Fatalf("targets(HandleBound, 39:9) = %v, want Store.Find among them", ts)
	}
	var got []string
	for _, e := range res.CallEdges {
		if e.Source == src.Signature {
			got = append(got, e.Target)
		}
	}
	if want := []string{wrapPkg + ".sqlStore.Find"}; !slices.Equal(got, want) {
		t.Errorf("VTA edges from HandleBound = %v, want %v", got, want)
	}
}
