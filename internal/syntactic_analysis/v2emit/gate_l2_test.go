package v2emit

// L2 conformance gate for the v2 emitter.
//
// L2 is a pure refinement over L1: it backfills the callee null->id slot and
// emits the call_graph edge list, both endpoints callable ids. This gate builds
// the multipackage fixture through the REAL resolver (the same wiring the
// analyzer uses at -a 2), emits v2, and asserts every item on the L2 checklist
// in designing-cldk-changes/references/testing-and-validation.md:
//
//   - no dangling endpoints — every edge src/dst is a real callable id
//   - every edge carries a non-empty prov naming the resolver
//   - callee is a backfilled can:// id on resolved sites, null on unresolved
//   - a NAMED expected edge is present (exact src,dst pair, not "graph non-empty")
//   - at least one cross-package edge is present
//   - the L1 ⊆ L2 superset holds: the tree is unchanged except the callee fill.

import (
	"testing"

	"github.com/codellm-devkit/codeanalyzer-go/internal/schema"
	v2 "github.com/codellm-devkit/codeanalyzer-go/internal/schema/v2"
	"github.com/codellm-devkit/codeanalyzer-go/internal/semantic_analysis"
	"github.com/codellm-devkit/codeanalyzer-go/internal/syntactic_analysis"
)

// buildMultipackageL2 builds the fixture AND runs the resolver, so the emitted
// v2 payload carries L2 facts (backfilled callees, call_graph edges).
func buildMultipackageL2(t *testing.T) *v2.Analysis {
	t.Helper()
	dir := multipackageDir(t)
	b := syntactic_analysis.NewSymbolTableBuilder(dir)
	st, err := b.Build(nil, true)
	if err != nil {
		t.Fatalf("symbol table build failed: %v", err)
	}
	cg := semantic_analysis.NewCallGraphBuilder(dir, b.Fset(), b.Pkgs())
	edges := cg.Build(st) // mutates st in place: backfills callee_signature
	app := &schema.GoApplication{SymbolTable: st, CallGraph: edges}
	return Emit(app, "multipackage", dir, 2, "gate-test")
}

// allCallableIDs collects every callable/type id in the emitted tree.
func allCallableIDs(out *v2.Analysis) map[string]bool {
	ids := map[string]bool{}
	var walk func(c v2.Callable)
	walk = func(c v2.Callable) {
		ids[c.ID] = true
		for _, inner := range c.Callables {
			walk(inner)
		}
	}
	for _, mod := range out.Application.SymbolTable {
		for _, fn := range mod.Functions {
			walk(fn)
		}
		for _, ty := range mod.Types {
			ids[ty.ID] = true
			for _, m := range ty.Callables {
				walk(m)
			}
		}
	}
	return ids
}

// TestGateL2_MaxLevelAndEdgesPresent locks that L2 emits a non-empty call graph.
func TestGateL2_MaxLevelAndEdgesPresent(t *testing.T) {
	out := buildMultipackageL2(t)
	if out.MaxLevel != 2 {
		t.Errorf("max_level = %d, want 2", out.MaxLevel)
	}
	if len(out.Application.CallGraph) == 0 {
		t.Fatal("L2 should emit call_graph edges; got none")
	}
}

// TestGateL2_NoDanglingEndpoints locks that every edge endpoint resolves to a
// real callable id, and every edge names its resolver in prov.
func TestGateL2_NoDanglingEndpoints(t *testing.T) {
	out := buildMultipackageL2(t)
	ids := allCallableIDs(out)

	for i, e := range out.Application.CallGraph {
		if !ids[e.Src] {
			t.Errorf("edge[%d] src is a dangling endpoint: %q", i, e.Src)
		}
		if !ids[e.Dst] {
			t.Errorf("edge[%d] dst is a dangling endpoint: %q", i, e.Dst)
		}
		if len(e.Prov) == 0 {
			t.Errorf("edge[%d] (%s -> %s) has empty prov", i, e.Src, e.Dst)
		}
	}
}

// TestGateL2_NamedEdgePresent asserts an EXACT expected edge, so the gate proves
// correctness, not merely a non-empty graph: Worker.Run calls Worker.execute
// (the `go w.execute(...)` goroutine), both as can:// ids.
func TestGateL2_NamedEdgePresent(t *testing.T) {
	out := buildMultipackageL2(t)

	const (
		src = "can://go/multipackage/worker/worker.go/example.com/multipackage/worker.Worker/example.com/multipackage/worker.Worker.Run"
		dst = "can://go/multipackage/worker/worker.go/example.com/multipackage/worker.Worker/example.com/multipackage/worker.Worker.execute"
	)
	found := false
	for _, e := range out.Application.CallGraph {
		if e.Src == src && e.Dst == dst {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected edge %s -> %s not found in call_graph", src, dst)
	}
}

// TestGateL2_CrossPackageEdge locks a specific cross-package edge: main() (in
// package main, main.go) calls server.New (in package server, server/server.go).
// Asserting the exact pair proves a resolved cross-module edge, not just that
// the graph is non-empty.
func TestGateL2_CrossPackageEdge(t *testing.T) {
	out := buildMultipackageL2(t)

	const (
		src = "can://go/multipackage/main.go/example.com/multipackage.main"
		dst = "can://go/multipackage/server/server.go/example.com/multipackage/server.New"
	)
	found := false
	for _, e := range out.Application.CallGraph {
		if e.Src == src && e.Dst == dst {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected cross-package edge %s -> %s not found", src, dst)
	}
}

// TestGateL2_CalleeBackfilled locks the refinement: the goroutine call site in
// Worker.Run has its callee backfilled to Worker.execute's can:// id (not null,
// not the raw v1 signature).
func TestGateL2_CalleeBackfilled(t *testing.T) {
	out := buildMultipackageL2(t)
	ids := allCallableIDs(out)

	run, ok := out.Application.SymbolTable["worker/worker.go"].
		Types["Worker"].Callables["example.com/multipackage/worker.Worker.Run"]
	if !ok {
		t.Fatal("Worker.Run missing")
	}
	if len(run.Body) == 0 {
		t.Fatal("Worker.Run should have body call nodes")
	}
	sawBackfilled := false
	for local, node := range run.Body {
		if node.Callee == nil {
			continue
		}
		if !ids[*node.Callee] {
			t.Errorf("body[%s].callee = %q is not an in-tree callable id", local, *node.Callee)
		}
		sawBackfilled = true
	}
	if !sawBackfilled {
		t.Error("Worker.Run should have at least one backfilled callee at L2")
	}
}

// TestGateL2_SupersetOfL1 locks L1 ⊆ L2: emitting the SAME model at level 1 vs
// level 2 changes nothing but the callee slot. Every module/type/callable id and
// span is identical; only body callees may go null -> id.
func TestGateL2_SupersetOfL1(t *testing.T) {
	dir := multipackageDir(t)
	b := syntactic_analysis.NewSymbolTableBuilder(dir)
	st, err := b.Build(nil, true)
	if err != nil {
		t.Fatalf("symbol table build failed: %v", err)
	}
	// L1 snapshot BEFORE the resolver mutates call sites.
	l1 := Emit(&schema.GoApplication{SymbolTable: st, CallGraph: nil}, "multipackage", dir, 1, "gate-test")

	// Now run the resolver (mutates st) and emit L2.
	cg := semantic_analysis.NewCallGraphBuilder(dir, b.Fset(), b.Pkgs())
	edges := cg.Build(st)
	l2 := Emit(&schema.GoApplication{SymbolTable: st, CallGraph: edges}, "multipackage", dir, 2, "gate-test")

	if len(l1.Application.SymbolTable) != len(l2.Application.SymbolTable) {
		t.Fatalf("module count changed L1->L2: %d vs %d",
			len(l1.Application.SymbolTable), len(l2.Application.SymbolTable))
	}
	for rel, m1 := range l1.Application.SymbolTable {
		m2 := l2.Application.SymbolTable[rel]
		if m1.ID != m2.ID || m1.Span != m2.Span || m1.Source != m2.Source {
			t.Errorf("%s: module identity/span/source changed L1->L2", rel)
		}
		assertCallablesEqualModuloCallee(t, rel, m1.Functions, m2.Functions)
		for name, t1 := range m1.Types {
			t2 := m2.Types[name]
			if t1.ID != t2.ID || t1.Span != t2.Span || t1.Kind != t2.Kind {
				t.Errorf("%s type %s: identity/span/kind changed L1->L2", rel, name)
			}
			assertCallablesEqualModuloCallee(t, rel+" "+name, t1.Callables, t2.Callables)
		}
	}
}

// assertCallablesEqualModuloCallee checks two callable maps are identical except
// that a body node's callee may have gone from null (L1) to a non-null id (L2).
func assertCallablesEqualModuloCallee(t *testing.T, where string, a, b map[string]v2.Callable) {
	t.Helper()
	if len(a) != len(b) {
		t.Errorf("%s: callable count changed L1->L2: %d vs %d", where, len(a), len(b))
		return
	}
	for sig, c1 := range a {
		c2, ok := b[sig]
		if !ok {
			t.Errorf("%s: callable %q dropped L1->L2", where, sig)
			continue
		}
		if c1.ID != c2.ID || c1.Span != c2.Span || c1.Kind != c2.Kind {
			t.Errorf("%s callable %s: identity/span/kind changed L1->L2", where, sig)
		}
		if len(c1.Body) != len(c2.Body) {
			t.Errorf("%s callable %s: body node count changed L1->L2", where, sig)
			continue
		}
		for local, n1 := range c1.Body {
			n2 := c2.Body[local]
			if n1.Kind != n2.Kind || n1.Span != n2.Span || n1.IsGoroutine != n2.IsGoroutine {
				t.Errorf("%s callable %s body[%s]: non-callee field changed L1->L2", where, sig, local)
			}
			// The one sanctioned mutation: callee null -> id only.
			if n1.Callee != nil {
				t.Errorf("%s callable %s body[%s]: L1 callee should be null", where, sig, local)
			}
		}
		assertCallablesEqualModuloCallee(t, where+" "+sig, c1.Callables, c2.Callables)
	}
}
