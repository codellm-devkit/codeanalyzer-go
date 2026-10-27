package v2emit

import (
	"testing"

	v2 "github.com/codellm-devkit/codeanalyzer-go/internal/schema/v2"
)

// l4Vertex is the one body node of owner with kind and of in a's tree.
func l4Vertex(t *testing.T, a *v2.Analysis, owner, kind, of string) string {
	t.Helper()
	var found []string
	for id, on := range bodyIndex(a) {
		if on.owner == owner && on.node.Kind == kind && on.node.Of == of {
			found = append(found, id)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%s has %d %s vertices of %q, want 1", owner, len(found), kind, of)
	}
	return found[0]
}

// hasVertex reports whether owner has a body node of kind and of in a's tree.
func hasVertex(a *v2.Analysis, owner, kind, of string) bool {
	for _, on := range bodyIndex(a) {
		if on.owner == owner && on.node.Kind == kind && on.node.Of == of {
			return true
		}
	}
	return false
}

// TestGateL4_FlowBehindSyntheticWrappers (F1): on testdata/dataflow/wrap,
// each handler's id reaches Exec's query formal — called directly through
// the interface, through a method value of it ((Store).Find$bound) and
// through a struct embedding it (a promotion wrapper) — and each handler's
// formal_out of the global sqlStore.Find writes exists.
func TestGateL4_FlowBehindSyntheticWrappers(t *testing.T) {
	a := buildL4(t, "dataflow/wrap").l4
	if len(a.Degraded) != 0 {
		t.Fatalf("wrap degraded: %+v", a.Degraded)
	}
	mod := v2.ModuleID(a.Application.ID, "wrap.go")
	pkg := "example.com/dataflow/wrap."
	fn := func(name string) string { return v2.CallableID(mod, pkg+name) }
	sink := l4Vertex(t, a, fn("Exec"), "formal_in", "query")
	g := newSDGGraph(a)
	for _, h := range []string{"HandleDirect", "HandleBound", "HandleDecorated"} {
		src := l4Vertex(t, a, fn(h), "formal_in", "id")
		if p := g.path(src, sink); p == nil {
			t.Errorf("%s's id does not reach Exec's query", h)
		}
		if !hasVertex(a, fn(h), "formal_out", "<global>:example.com/dataflow/wrap::lastQuery") {
			t.Errorf("%s has no formal_out of the global sqlStore.Find writes", h)
		}
	}
}

// TestGateL4_FlowThroughLibraryStorage (F3): on testdata/dataflow/libstore,
// a Store put in a context.Context or a sync.Map comes back out of library
// code and is called; each handler's id reaches Exec's query formal through
// the CHA complement's sqlStore.Find, and the handler's formal_out of the
// global sqlStore.Find writes exists.
func TestGateL4_FlowThroughLibraryStorage(t *testing.T) {
	a := buildL4(t, "dataflow/libstore").l4
	if len(a.Degraded) != 0 {
		t.Fatalf("libstore degraded: %+v", a.Degraded)
	}
	mod := v2.ModuleID(a.Application.ID, "libstore.go")
	pkg := "example.com/dataflow/libstore."
	fn := func(name string) string { return v2.CallableID(mod, pkg+name) }
	sink := l4Vertex(t, a, fn("Exec"), "formal_in", "query")
	g := newSDGGraph(a)
	for _, h := range []string{"HandleCtx", "HandleMap"} {
		src := l4Vertex(t, a, fn(h), "formal_in", "id")
		if p := g.path(src, sink); p == nil {
			t.Errorf("%s's id does not reach Exec's query", h)
		}
		if !hasVertex(a, fn(h), "formal_out", "<global>:example.com/dataflow/libstore::lastQuery") {
			t.Errorf("%s has no formal_out of the global sqlStore.Find writes", h)
		}
	}
}
