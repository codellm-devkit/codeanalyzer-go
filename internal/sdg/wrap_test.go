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
