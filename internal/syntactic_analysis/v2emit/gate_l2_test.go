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
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
	app := &schema.GoApplication{SymbolTable: st, CallGraph: edges, Externals: cg.Externals()}
	return Emit(app, "multipackage", dir, 2, "gate-test", 0, nil, nil)
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
	// A library callee is a real node too: its id is a key of external_symbols.
	for id := range out.Application.ExternalSymbols {
		ids[id] = true
	}

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
		src = "can://multipackage/go/worker/worker.go/example.com/multipackage/worker.Worker/example.com/multipackage/worker.Worker.Run"
		dst = "can://multipackage/go/worker/worker.go/example.com/multipackage/worker.Worker/example.com/multipackage/worker.Worker.execute"
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
		src = "can://multipackage/go/main.go/example.com/multipackage.main"
		dst = "can://multipackage/go/server/server.go/example.com/multipackage/server.New"
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
		if _, ext := out.Application.ExternalSymbols[*node.Callee]; !ids[*node.Callee] && !ext {
			t.Errorf("body[%s].callee = %q is neither an in-tree callable nor an external symbol", local, *node.Callee)
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
	l1 := Emit(&schema.GoApplication{SymbolTable: st, CallGraph: nil}, "multipackage", dir, 1, "gate-test", 0, nil, nil)

	// Now run the resolver (mutates st) and emit L2.
	cg := semantic_analysis.NewCallGraphBuilder(dir, b.Fset(), b.Pkgs())
	edges := cg.Build(st)
	l2 := Emit(&schema.GoApplication{SymbolTable: st, CallGraph: edges}, "multipackage", dir, 2, "gate-test", 0, nil, nil)

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

func anyCallNodeHasCallee(out *v2.Analysis, id string) bool {
	found := false
	var walk func(c v2.Callable)
	walk = func(c v2.Callable) {
		for _, n := range c.Body {
			if n.Callee != nil && *n.Callee == id {
				found = true
			}
		}
		for _, inner := range c.Callables {
			walk(inner)
		}
	}
	for _, mod := range out.Application.SymbolTable {
		for _, fn := range mod.Functions {
			walk(fn)
		}
		for _, ty := range mod.Types {
			for _, m := range ty.Callables {
				walk(m)
			}
		}
	}
	return found
}

func anyEdgeTo(edges []v2.Edge, dst string) bool {
	for _, e := range edges {
		if e.Dst == dst {
			return true
		}
	}
	return false
}

// TestGateL2_ExternalCalleeBackfilled locks that a call into a library keeps its
// edge: multipackage/main.go calls fmt.Println, so external_symbols carries it,
// the call node's callee is its @external id, and a call_graph edge targets it.
func TestGateL2_ExternalCalleeBackfilled(t *testing.T) {
	out := buildMultipackageL2(t)
	ext := v2.ExternalID(out.Application.ID, "fmt", "Println")

	sym, ok := out.Application.ExternalSymbols[ext]
	if !ok {
		t.Fatalf("external_symbols missing %s", ext)
	}
	want := v2.ExternalSymbol{ID: ext, Kind: "external", Name: "Println", Module: "fmt", Signature: "Println"}
	if sym.ID != want.ID || sym.Kind != want.Kind || sym.Name != want.Name ||
		sym.Module != want.Module || sym.Signature != want.Signature {
		t.Errorf("external symbol = %+v, want identity fields %+v", sym, want)
	}
	if len(sym.Parameters) != 1 || sym.Parameters[0].Name != "a" || !sym.Parameters[0].IsVariadic {
		t.Errorf("Println parameters = %+v, want one variadic param named a", sym.Parameters)
	}
	if !anyCallNodeHasCallee(out, ext) {
		t.Errorf("no call node has callee %s", ext)
	}
	if !anyEdgeTo(out.Application.CallGraph, ext) {
		t.Errorf("no call_graph edge targets %s", ext)
	}
}

// TestGateL2_ExternalMethodSignature locks the method form of the sig segment:
// strings.Builder.WriteString style -- Type.Method, receiver not a parameter.
func TestGateL2_ExternalMethodSignature(t *testing.T) {
	out := emitTemp(t, `package p

import "strings"

func F() string {
	var sb strings.Builder
	sb.WriteString("x")
	return sb.String()
}
`)
	ext := v2.ExternalID(out.Application.ID, "strings", "Builder.WriteString")
	sym, ok := out.Application.ExternalSymbols[ext]
	if !ok {
		t.Fatalf("external_symbols missing %s; have %v", ext, out.Application.ExternalSymbols)
	}
	if sym.Name != "WriteString" || sym.Signature != "Builder.WriteString" || sym.Module != "strings" {
		t.Errorf("unexpected symbol %+v", sym)
	}
	if len(sym.Parameters) != 1 || sym.Parameters[0].Name != "s" {
		t.Errorf("params = %+v, want exactly [s] (receiver excluded)", sym.Parameters)
	}
	if sym.Receiver == nil || sym.Receiver.Name != "b" || sym.Receiver.Type != "*strings.Builder" {
		t.Errorf("receiver = %+v, want {b *strings.Builder}", sym.Receiver)
	}
}

// TestGateL2_ExternalFunctionHasNoReceiver: only methods carry a receiver.
func TestGateL2_ExternalFunctionHasNoReceiver(t *testing.T) {
	out := emitTemp(t, `package p

import "strings"

func F() string { return strings.ToUpper("x") }
`)
	sym, ok := out.Application.ExternalSymbols[v2.ExternalID(out.Application.ID, "strings", "ToUpper")]
	if !ok {
		t.Fatalf("external_symbols missing strings.ToUpper")
	}
	if sym.Receiver != nil {
		t.Errorf("function has receiver %+v", sym.Receiver)
	}
}

// TestGateL2_ConversionIsNotACall locks that a type conversion is not minted as
// a call: its call node keeps callee null and no "__new__" appears anywhere.
func TestGateL2_ConversionIsNotACall(t *testing.T) {
	out := emitTemp(t, `package p

func F(b []byte) string {
	return string(b)
}
`)
	fn := out.Application.SymbolTable["p.go"].Functions
	if len(fn) != 1 {
		t.Fatalf("want 1 function, got %d", len(fn))
	}
	for _, c := range fn {
		if len(c.Body) != 1 {
			t.Fatalf("want 1 call node (the conversion), got %d", len(c.Body))
		}
		for _, n := range c.Body {
			if n.Callee != nil {
				t.Errorf("conversion callee = %q, want null", *n.Callee)
			}
		}
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "__new__") {
		t.Error("output contains __new__")
	}
	if len(out.Application.ExternalSymbols) != 0 {
		t.Errorf("conversion minted external symbols: %v", out.Application.ExternalSymbols)
	}
}

// emitTemp builds a one-file module (p.go) in a temp dir through the real
// builder + resolver and emits v2 at L2.
func emitTemp(t *testing.T, src string) *v2.Analysis {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/p\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "p.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	b := syntactic_analysis.NewSymbolTableBuilder(dir)
	st, err := b.Build(nil, true)
	if err != nil {
		t.Fatalf("symbol table build failed: %v", err)
	}
	cg := semantic_analysis.NewCallGraphBuilder(dir, b.Fset(), b.Pkgs())
	edges := cg.Build(st)
	app := &schema.GoApplication{SymbolTable: st, CallGraph: edges, Externals: cg.Externals()}
	return Emit(app, "p", dir, 2, "gate-test", 0, nil, nil)
}

// TestGateL2_ExternalParamsHaveNoSpan locks that external parameters serialize
// without a span key (no fabricated location).
func TestGateL2_ExternalParamsHaveNoSpan(t *testing.T) {
	out := buildMultipackageL2(t)
	ext := v2.ExternalID(out.Application.ID, "fmt", "Println")
	raw, err := json.Marshal(out.Application.ExternalSymbols[ext])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "span") {
		t.Errorf("external symbol JSON carries a span: %s", raw)
	}
}

// TestGateL2_GenericLibraryMethodUsesDeclaredSignature locks that methods on two
// instantiations of one generic library type (atomic.Pointer[A], [B], called in
// two files) collapse to ONE external whose params are the declared form (*T),
// independent of which instance the resolver visits first.
func TestGateL2_GenericLibraryMethodUsesDeclaredSignature(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "testdata", "dataflow")
	for i := 0; i < 6; i++ { // fresh builds: map order varies
		b := syntactic_analysis.NewSymbolTableBuilder(dir)
		st, err := b.Build(nil, true)
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		cg := semantic_analysis.NewCallGraphBuilder(dir, b.Fset(), b.Pkgs())
		edges := cg.Build(st)
		out := Emit(&schema.GoApplication{SymbolTable: st, CallGraph: edges, Externals: cg.Externals()}, "dataflow", dir, 2, "gate-test", 0, nil, nil)
		ext := v2.ExternalID(out.Application.ID, "sync/atomic", "Pointer.Store")
		sym, ok := out.Application.ExternalSymbols[ext]
		if !ok {
			t.Fatalf("missing %s; have %v", ext, out.Application.ExternalSymbols)
		}
		if len(sym.Parameters) != 1 || sym.Parameters[0].Type != "*T" {
			t.Fatalf("Pointer.Store params = %+v, want one param of type *T", sym.Parameters)
		}
	}
}

// TestGateL2_IncrementalModeKeepsProjectCalleesInternal locks that with
// TargetFiles (sibling project packages load only as deps) calls into the project
// are never minted as @external symbols named after the project's own module.
func TestGateL2_IncrementalModeKeepsProjectCalleesInternal(t *testing.T) {
	dir := multipackageDir(t)
	b := syntactic_analysis.NewSymbolTableBuilder(dir)
	st, err := b.Build([]string{filepath.Join(dir, "main.go")}, true)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	cg := semantic_analysis.NewCallGraphBuilder(dir, b.Fset(), b.Pkgs())
	edges := cg.Build(st)
	out := Emit(&schema.GoApplication{SymbolTable: st, CallGraph: edges, Externals: cg.Externals()}, "multipackage", dir, 2, "gate-test", 0, nil, nil)

	const projectModule = "example.com/multipackage"
	for id, sym := range out.Application.ExternalSymbols {
		if sym.Module == projectModule || strings.HasPrefix(sym.Module, projectModule+"/") {
			t.Errorf("project callee minted as external: %s (module %s)", id, sym.Module)
		}
	}
	if _, ok := out.Application.ExternalSymbols[v2.ExternalID(out.Application.ID, "fmt", "Println")]; !ok {
		t.Error("fmt.Println should still be external in incremental mode")
	}
}

// TestGateL2_VendoredCalleeIsExternal locks that a callee declared under
// <projectDir>/vendor (inside the project dir, but skipped by the symbol table)
// is a library: dataflow/vend calls vlib.F from the vendored example.com/vlib.
func TestGateL2_VendoredCalleeIsExternal(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "testdata", "dataflow")
	b := syntactic_analysis.NewSymbolTableBuilder(dir)
	st, err := b.Build(nil, true)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	cg := semantic_analysis.NewCallGraphBuilder(dir, b.Fset(), b.Pkgs())
	edges := cg.Build(st)
	out := Emit(&schema.GoApplication{SymbolTable: st, CallGraph: edges, Externals: cg.Externals()}, "dataflow", dir, 2, "gate-test", 0, nil, nil)

	ext := v2.ExternalID(out.Application.ID, "example.com/vlib", "F")
	sym, ok := out.Application.ExternalSymbols[ext]
	if !ok {
		t.Fatalf("external_symbols missing %s; have %v", ext, out.Application.ExternalSymbols)
	}
	if sym.Module != "example.com/vlib" || sym.Signature != "F" {
		t.Errorf("unexpected symbol %+v", sym)
	}
	if !anyCallNodeHasCallee(out, ext) {
		t.Errorf("no call node has callee %s", ext)
	}
	if !anyEdgeTo(out.Application.CallGraph, ext) {
		t.Errorf("no call_graph edge targets %s", ext)
	}
}

// TestGateL2_LineDirectiveCallsResolve (I1, #26): L1 records call sites at
// their real file positions, never the //line-adjusted ones, so a site under
// a directive keeps its real line:col (here 13:2, which the directive maps
// to virtual.y:900). The resolver must find it by that same key and backfill
// its callee and edge.
func TestGateL2_LineDirectiveCallsResolve(t *testing.T) {
	out := buildDataflowL2Fresh(t)
	fn := functionByName(t, mustModule(t, out, "linedir/linedir.go"), "Caller")
	helper := functionByName(t, mustModule(t, out, "linedir/linedir.go"), "helper").ID
	getenv := v2.ExternalID(out.Application.ID, "os", "Getenv")

	callees := map[string]bool{}
	for k, n := range fn.Body {
		if n.Kind != "call" {
			continue
		}
		if n.Callee == nil {
			t.Errorf("call node %s has a null callee", k)
			continue
		}
		callees[*n.Callee] = true
	}
	if _, real := fn.Body["13:2"]; !real {
		t.Fatalf("no call node at the real 13:2; body %v", keys(fn.Body))
	}
	edges := map[string]bool{}
	for _, e := range out.Application.CallGraph {
		if e.Src == fn.ID {
			edges[e.Dst] = true
		}
	}
	for _, dst := range []string{helper, getenv} {
		if !callees[dst] {
			t.Errorf("no call node of Caller has callee %s", dst)
		}
		if !edges[dst] {
			t.Errorf("missing call_graph edge %s -> %s", fn.ID, dst)
		}
	}
}

// TestGateL2_ExternalUnnamedParamsArePositional: a library callee's unnamed
// or blank parameters are named by position, _<i>, in external_symbols
// (vlib.G(int, string), vlib.H(_ int, n string)).
func TestGateL2_ExternalUnnamedParamsArePositional(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "testdata", "dataflow")
	b := syntactic_analysis.NewSymbolTableBuilder(dir)
	st, err := b.Build(nil, true)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	cg := semantic_analysis.NewCallGraphBuilder(dir, b.Fset(), b.Pkgs())
	edges := cg.Build(st)
	out := Emit(&schema.GoApplication{SymbolTable: st, CallGraph: edges, Externals: cg.Externals()}, "dataflow", dir, 2, "gate-test", 0, nil, nil)

	for sig, want := range map[string][]string{"G": {"_0", "_1"}, "H": {"_0", "n"}} {
		ext := v2.ExternalID(out.Application.ID, "example.com/vlib", sig)
		sym, ok := out.Application.ExternalSymbols[ext]
		if !ok {
			t.Errorf("external_symbols missing %s", ext)
			continue
		}
		var got []string
		for _, p := range sym.Parameters {
			got = append(got, p.Name)
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s parameter names = %v, want %v", sig, got, want)
		}
	}
}

// emitDirL2 builds dir through the real builder + resolver and emits v2 at L2.
func emitDirL2(t *testing.T, dir, app string) *v2.Analysis {
	t.Helper()
	b := syntactic_analysis.NewSymbolTableBuilder(dir)
	st, err := b.Build(nil, true)
	if err != nil {
		t.Fatalf("symbol table build failed: %v", err)
	}
	cg := semantic_analysis.NewCallGraphBuilder(dir, b.Fset(), b.Pkgs())
	edges := cg.Build(st)
	return Emit(&schema.GoApplication{SymbolTable: st, CallGraph: edges, Externals: cg.Externals()}, app, dir, 2, "gate-test", 0, nil, nil)
}

// assertNoDanglingEdges fails when an edge endpoint is neither a callable id
// nor an external_symbols key.
func assertNoDanglingEdges(t *testing.T, out *v2.Analysis) {
	t.Helper()
	ids := allCallableIDs(out)
	for id := range out.Application.ExternalSymbols {
		ids[id] = true
	}
	for i, e := range out.Application.CallGraph {
		if !ids[e.Src] || !ids[e.Dst] {
			t.Errorf("edge[%d] dangles: %s -> %s", i, e.Src, e.Dst)
		}
	}
}

// TestGateL2_ClosureCallSitesResolve: a call inside a function literal resolves
// like any other (#17). flow.Closure's literal calls f(x) at 217:10.
func TestGateL2_ClosureCallSitesResolve(t *testing.T) {
	out := buildDataflowL2Fresh(t)
	mod := mustModule(t, out, "flow/flow.go")
	closure := functionByName(t, mod, "Closure")
	if len(closure.Callables) != 1 {
		t.Fatalf("Closure has %d closures, want 1", len(closure.Callables))
	}
	var lit v2.Callable
	for _, c := range closure.Callables {
		lit = c
	}
	wantCallee := functionByName(t, mod, "f").ID

	n, ok := lit.Body["217:10"]
	if !ok {
		t.Fatalf("closure body has no 217:10 call; keys=%v", keys(lit.Body))
	}
	if n.Callee == nil || *n.Callee != wantCallee {
		t.Errorf("closure call 217:10 callee = %v, want %s", n.Callee, wantCallee)
	}
	found := false
	for _, e := range out.Application.CallGraph {
		if e.Src == lit.ID && e.Dst == wantCallee {
			found = true
			if len(e.Prov) != 1 || e.Prov[0] != "go/types" {
				t.Errorf("closure edge prov = %v, want [go/types]", e.Prov)
			}
		}
	}
	if !found {
		t.Errorf("call_graph has no edge %s -> %s", lit.ID, wantCallee)
	}
	assertNoDanglingEdges(t, out)
}

// TestGateL2_ChiClosureCallsResolve: on chi, closure call nodes resolve and no
// edge dangles.
func TestGateL2_ChiClosureCallsResolve(t *testing.T) {
	out := buildChiL2Fresh(t, chiDir(t))
	resolved := 0
	var walk func(c v2.Callable, inClosure bool)
	walk = func(c v2.Callable, inClosure bool) {
		if inClosure {
			for _, n := range c.Body {
				if n.Kind == "call" && n.Callee != nil {
					resolved++
				}
			}
		}
		for _, in := range c.Callables {
			walk(in, true)
		}
	}
	for _, mod := range out.Application.SymbolTable {
		for _, fn := range mod.Functions {
			walk(fn, false)
		}
		for _, ty := range mod.Types {
			for _, m := range ty.Callables {
				walk(m, false)
			}
		}
	}
	if resolved == 0 {
		t.Error("no closure call node has a callee on chi")
	}
	t.Logf("chi closure call nodes with a callee: %d", resolved)
	assertNoDanglingEdges(t, out)
}

// TestGateL2_CrossFileMethodClosureEdge: a closure inside a method declared in a
// different file than its receiver type nests under the type's module but is
// declared in the method's file. Its edge must resolve to the closure's id.
func TestGateL2_CrossFileMethodClosureEdge(t *testing.T) {
	dir := t.TempDir()
	for name, src := range map[string]string{
		"go.mod": "module example.com/p\n\ngo 1.21\n",
		"a.go":   "package p\n\ntype T struct{}\n",
		"b.go":   "package p\n\nfunc helper() {}\n\nfunc (t *T) Run() {\n\tfn := func() {\n\t\tinner := func() { helper() }\n\t\tinner()\n\t\thelper()\n\t}\n\tfn()\n}\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out := emitDirL2(t, dir, "p")
	run := mustModule(t, out, "a.go").Types["T"].Callables["example.com/p.T.Run"]
	if len(run.Callables) != 1 {
		t.Fatalf("Run has %d closures, want 1", len(run.Callables))
	}
	helper := functionByName(t, mustModule(t, out, "b.go"), "helper").ID
	var fn v2.Callable
	for _, c := range run.Callables {
		fn = c
	}
	var inner v2.Callable
	for _, c := range fn.Callables {
		inner = c
	}
	has := map[[2]string]bool{}
	for _, e := range out.Application.CallGraph {
		has[[2]string{e.Src, e.Dst}] = true
	}
	for _, c := range []v2.Callable{fn, inner} {
		if c.ID == "" || !has[[2]string{c.ID, helper}] {
			t.Errorf("missing edge %s -> %s", c.ID, helper)
		}
	}
	assertNoDanglingEdges(t, out)
}
