package v2emit

// L1 conformance gate for the v2 emitter.
//
// This is the fixture gate the codeanalyzer-backend ladder requires before L1
// advances: it locks the v2 L1 OUTPUT SHAPE against a known multi-file fixture,
// asserting concrete values (not just "a key exists") for every item on the L1
// coverage checklist in
// designing-cldk-changes/references/testing-and-validation.md:
//
//   - a multi-file compilation unit (four modules keyed by relative path)
//   - exported AND unexported symbols (Worker.Run vs Worker.execute)
//   - an interface type collapsed into kind=interface (Processor)
//   - error_channel populated from an error-typed return (Processor.Process)
//   - a variadic parameter (Combine(results ...Result))
//   - a language-specific call flag (is_goroutine on `go w.execute(...)`)
//   - the sanctioned callee:null refinement slot present at L1
//   - can:// durable ids at module/type/callable depth
//   - the SUPERSET gate: every v1 fact survives into v2 modulo sanctioned drops
//     (is_constructor_call is dropped; per-node `code` folds into module.source).
//
// If any of these regress, this gate fails and the level does not advance.

import (
	"encoding/json"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/codellm-devkit/codeanalyzer-go/internal/schema"
	v2 "github.com/codellm-devkit/codeanalyzer-go/internal/schema/v2"
	"github.com/codellm-devkit/codeanalyzer-go/internal/semantic_analysis"
	"github.com/codellm-devkit/codeanalyzer-go/internal/syntactic_analysis"
)

// multipackageDir returns the absolute path to the multipackage testdata fixture.
func multipackageDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	abs, err := filepath.Abs(filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "testdata", "multipackage"))
	if err != nil {
		t.Fatalf("resolving fixture dir: %v", err)
	}
	return abs
}

// buildMultipackageV2 runs the real symbol-table builder over the multipackage
// fixture and emits the v2 L1 payload. skipTests=true keeps *_test.go out so the
// module set is deterministic.
func buildMultipackageV2(t *testing.T) *v2.Analysis {
	t.Helper()
	dir := multipackageDir(t)
	b := syntactic_analysis.NewSymbolTableBuilder(dir)
	st, err := b.Build(nil, true)
	if err != nil {
		t.Fatalf("symbol table build failed: %v", err)
	}
	app := &schema.GoApplication{SymbolTable: st, CallGraph: []schema.GoCallEdge{}}
	return Emit(app, "multipackage", dir, 1, "gate-test", 0, nil)
}

// mustModule fetches a module by relative path or fails.
func mustModule(t *testing.T, out *v2.Analysis, rel string) v2.Module {
	t.Helper()
	mod, ok := out.Application.SymbolTable[rel]
	if !ok {
		t.Fatalf("module %q missing; modules=%v", rel, keys(out.Application.SymbolTable))
	}
	return mod
}

// TestGateL1_ModuleSet locks the exact multi-file compilation unit: the four
// non-test source files, each a module keyed by its relative path.
func TestGateL1_ModuleSet(t *testing.T) {
	out := buildMultipackageV2(t)

	want := []string{
		"main.go",
		"server/middleware.go",
		"server/server.go",
		"worker/worker.go",
	}
	for _, rel := range want {
		mod := mustModule(t, out, rel)
		if mod.Kind != "module" {
			t.Errorf("%s: kind = %q, want module", rel, mod.Kind)
		}
		wantID := "can://multipackage/go/" + rel
		if mod.ID != wantID {
			t.Errorf("%s: id = %q, want %q", rel, mod.ID, wantID)
		}
		if mod.Source == "" {
			t.Errorf("%s: source is empty; every module carries its whole file text", rel)
		}
		// The module span must slice back to the whole source (source-once invariant).
		if got := mod.Span.Slice(mod.Source); got != mod.Source {
			t.Errorf("%s: module span does not cover whole source (%d vs %d bytes)", rel, len(got), len(mod.Source))
		}
	}
}

// TestGateL1_InterfaceKind locks the Processor interface: is_interface collapses
// into kind=interface, and its method resolves as a callable under the type.
func TestGateL1_InterfaceKind(t *testing.T) {
	out := buildMultipackageV2(t)
	worker := mustModule(t, out, "worker/worker.go")

	proc, ok := worker.Types["Processor"]
	if !ok {
		t.Fatalf("Processor type missing; types=%v", keys(worker.Types))
	}
	if proc.Kind != "interface" {
		t.Errorf("Processor.kind = %q, want interface", proc.Kind)
	}
	const procMethod = "example.com/multipackage/worker.Processor.Process"
	m, ok := proc.Callables[procMethod]
	if !ok {
		t.Fatalf("Processor.Process missing; callables=%v", keys(proc.Callables))
	}
	if m.Kind != "method" {
		t.Errorf("Process.kind = %q, want method", m.Kind)
	}
	// error_channel is populated from the (Result, error) return.
	if !contains(m.ErrorChannel, "error") {
		t.Errorf("Process.error_channel = %v, want to contain \"error\"", m.ErrorChannel)
	}
}

// TestGateL1_ExportedAndUnexported locks that both an exported (Run) and an
// unexported (execute) method survive, each a method callable under Worker.
func TestGateL1_ExportedAndUnexported(t *testing.T) {
	out := buildMultipackageV2(t)
	worker := mustModule(t, out, "worker/worker.go")

	wtype, ok := worker.Types["Worker"]
	if !ok {
		t.Fatalf("Worker type missing; types=%v", keys(worker.Types))
	}
	const (
		runID  = "example.com/multipackage/worker.Worker.Run"
		execID = "example.com/multipackage/worker.Worker.execute"
	)
	for _, sig := range []string{runID, execID} {
		m, ok := wtype.Callables[sig]
		if !ok {
			t.Fatalf("method %q missing; callables=%v", sig, keys(wtype.Callables))
		}
		if m.Kind != "method" {
			t.Errorf("%s.kind = %q, want method", sig, m.Kind)
		}
	}
}

// TestGateL1_VariadicParameter locks the variadic-parameter fact:
// Combine(results ...Result) → is_variadic=true on the sole parameter.
func TestGateL1_VariadicParameter(t *testing.T) {
	out := buildMultipackageV2(t)
	worker := mustModule(t, out, "worker/worker.go")

	const combineID = "example.com/multipackage/worker.Combine"
	comb, ok := worker.Functions[combineID]
	if !ok {
		t.Fatalf("Combine function missing; functions=%v", keys(worker.Functions))
	}
	if len(comb.Parameters) != 1 {
		t.Fatalf("Combine should have 1 parameter; got %d", len(comb.Parameters))
	}
	if !comb.Parameters[0].IsVariadic {
		t.Errorf("Combine parameter %q should be variadic", comb.Parameters[0].Name)
	}
}

// TestGateL1_GoroutineCallNode locks the language-specific call flag: Worker.Run
// contains `go w.execute(...)`, which must surface as a body call node with
// is_goroutine=true AND callee null at L1.
func TestGateL1_GoroutineCallNode(t *testing.T) {
	out := buildMultipackageV2(t)
	worker := mustModule(t, out, "worker/worker.go")

	run, ok := worker.Types["Worker"].Callables["example.com/multipackage/worker.Worker.Run"]
	if !ok {
		t.Fatal("Worker.Run missing")
	}
	if len(run.Body) == 0 {
		t.Fatal("Worker.Run should have body call nodes")
	}
	sawGoroutine := false
	for local, node := range run.Body {
		if node.Kind != "call" {
			t.Errorf("body[%s].kind = %q, want call", local, node.Kind)
		}
		if node.Callee != nil {
			t.Errorf("body[%s].callee should be null at L1; got %q", local, *node.Callee)
		}
		if node.IsGoroutine {
			sawGoroutine = true
		}
	}
	if !sawGoroutine {
		t.Error("Worker.Run should have a call node with is_goroutine=true (go w.execute(...))")
	}
}

// TestGateL1_CrossPackageStructure locks that a cross-package caller (main.go
// calling into server/ and worker/) is present as its own module with body call
// nodes — the compilation unit spans packages, not a single file.
func TestGateL1_CrossPackageStructure(t *testing.T) {
	out := buildMultipackageV2(t)
	main := mustModule(t, out, "main.go")

	if main.Package != "main" {
		t.Errorf("main.go package = %q, want main", main.Package)
	}
	// main() drives the cross-package calls; its body must carry call nodes.
	var mainFn v2.Callable
	found := false
	for _, fn := range main.Functions {
		if strings.HasSuffix(fn.Signature, ".main") {
			mainFn, found = fn, true
		}
	}
	if !found {
		t.Fatalf("main function missing; functions=%v", keys(main.Functions))
	}
	if len(mainFn.Body) == 0 {
		t.Error("main() should have body call nodes for its cross-package calls")
	}
}

// TestGateL1_CrossFileMethodSourceFile locks the source_file amendment
// (CLAUDE.md § Schema decisions → callable.source_file): a method declared in a
// different file than its receiver type stays nested under the type, but its
// span.bytes index its OWN file's source and it carries source_file naming that
// file. In the fixture, Server is declared in server/server.go while its method
// (s Server) Describe() is declared in server/middleware.go.
//
// The text-recovery rule is the contract: a node's text =
// symbol_table[source_file].source[span.bytes] when source_file is present. This
// test slices the declaring module's source by the span and asserts it yields
// the method text — the check that regressed to "" before the fix (empty span
// against the wrong file's source).
func TestGateL1_CrossFileMethodSourceFile(t *testing.T) {
	out := buildMultipackageV2(t)

	// Server's type node lives in server/server.go.
	server := mustModule(t, out, "server/server.go")
	stype, ok := server.Types["Server"]
	if !ok {
		t.Fatalf("Server type missing; types=%v", keys(server.Types))
	}
	const describeID = "example.com/multipackage/server.Server.Describe"
	m, ok := stype.Callables[describeID]
	if !ok {
		t.Fatalf("Server.Describe missing; callables=%v", keys(stype.Callables))
	}

	// It is declared in a different file, so source_file must name that file.
	const declFile = "server/middleware.go"
	if m.SourceFile != declFile {
		t.Errorf("Describe.source_file = %q, want %q (declared apart from its type)", m.SourceFile, declFile)
	}

	// The span must be non-empty (the pre-fix bug collapsed it to [n,n]).
	if m.Span.Bytes[0] >= m.Span.Bytes[1] {
		t.Errorf("Describe span is empty %v — cross-file method span not computed against its own file", m.Span.Bytes)
	}

	// Text-recovery rule: slice the DECLARING module's source by the span.
	decl := mustModule(t, out, declFile)
	text := m.Span.Slice(decl.Source)
	if !strings.Contains(text, "func (s Server) Describe()") {
		t.Errorf("Describe span over %s does not slice to the method text; got %q", declFile, text)
	}

	// Negative: an in-file method (Server.Addr, declared in server.go with its
	// type) must NOT carry source_file — absence encodes "same file".
	addr, ok := stype.Callables["example.com/multipackage/server.Server.Addr"]
	if !ok {
		t.Fatalf("Server.Addr missing; callables=%v", keys(stype.Callables))
	}
	if addr.SourceFile != "" {
		t.Errorf("Addr.source_file = %q, want empty (declared in its type's own file)", addr.SourceFile)
	}
	if got := addr.Span.Slice(server.Source); !strings.Contains(got, "func (s *Server) Addr()") {
		t.Errorf("Addr span over its own module does not slice to the method text; got %q", got)
	}
}

// TestGateL1_Envelope locks the manifest envelope at L1.
func TestGateL1_Envelope(t *testing.T) {
	out := buildMultipackageV2(t)

	if out.SchemaVersion != v2.SchemaVersion {
		t.Errorf("schema_version = %q, want %q", out.SchemaVersion, v2.SchemaVersion)
	}
	if out.Language != "go" {
		t.Errorf("language = %q, want go", out.Language)
	}
	if out.MaxLevel != 1 {
		t.Errorf("max_level = %d, want 1", out.MaxLevel)
	}
	if out.Application.ID != "can://multipackage" {
		t.Errorf("application.id = %q, want can://multipackage", out.Application.ID)
	}
	// L1: no call_graph edges yet.
	if len(out.Application.CallGraph) != 0 {
		t.Errorf("L1 call_graph should be empty; got %d edges", len(out.Application.CallGraph))
	}
}

// TestGateL1_SupersetOfV1 is the superset gate: every v1 fact must survive into
// v2 modulo the sanctioned drops. It walks the SAME v1 model the emitter walked
// and asserts each callable/type/function reappears in v2, and that the
// sanctioned drops are gone from the serialized payload.
func TestGateL1_SupersetOfV1(t *testing.T) {
	dir := multipackageDir(t)
	b := syntactic_analysis.NewSymbolTableBuilder(dir)
	st, err := b.Build(nil, true)
	if err != nil {
		t.Fatalf("symbol table build failed: %v", err)
	}
	app := &schema.GoApplication{SymbolTable: st, CallGraph: []schema.GoCallEdge{}}
	out := Emit(app, "multipackage", dir, 1, "gate-test", 0, nil)

	// Every v1 file, type, method and function reappears in v2.
	for rel, v1file := range st {
		mod, ok := out.Application.SymbolTable[rel]
		if !ok {
			t.Errorf("v1 file %q dropped from v2 symbol_table", rel)
			continue
		}
		for name := range v1file.Types {
			if _, ok := mod.Types[name]; !ok {
				t.Errorf("%s: v1 type %q dropped from v2", rel, name)
			}
			for msig := range v1file.Types[name].Methods {
				if _, ok := mod.Types[name].Callables[msig]; !ok {
					t.Errorf("%s: v1 method %q dropped from v2 type %q", rel, msig, name)
				}
			}
		}
		for sig := range v1file.Functions {
			if _, ok := mod.Functions[sig]; !ok {
				t.Errorf("%s: v1 function %q dropped from v2", rel, sig)
			}
		}
	}

	// Sanctioned drops must be absent from the serialized payload.
	data, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	if strings.Contains(string(data), `"is_constructor_call"`) {
		t.Error("v2 payload should not carry the dropped is_constructor_call")
	}
	// The sanctioned callee:null slot must be present at L1.
	if !strings.Contains(string(data), `"callee":null`) {
		t.Error("expected the sanctioned callee:null refinement slot at L1")
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// ─── L1 fixes: type kinds, defer marker, co-positioned calls ─────────────────

// buildDataflowL1V2 emits the L1 payload for the testdata/dataflow fixture.
func buildDataflowL1V2(t *testing.T) *v2.Analysis {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	dir, err := filepath.Abs(filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "testdata", "dataflow"))
	if err != nil {
		t.Fatalf("resolving fixture dir: %v", err)
	}
	b := syntactic_analysis.NewSymbolTableBuilder(dir)
	st, err := b.Build(nil, true)
	if err != nil {
		t.Fatalf("symbol table build failed: %v", err)
	}
	app := &schema.GoApplication{SymbolTable: st, CallGraph: []schema.GoCallEdge{}}
	return Emit(app, "dataflow", dir, 1, "gate-test", 0, nil)
}

func TestGateL1_TypeKindsAliasAndDefined(t *testing.T) {
	mod := mustModule(t, buildDataflowL1V2(t), "l1/l1.go")
	want := map[string]string{"Celsius": "defined", "Alias": "alias", "S": "struct",
		"Base": "struct", "MyErr": "struct", "I1": "interface", "I2": "interface"}
	for name, kind := range want {
		ty, ok := mod.Types[name]
		if !ok {
			t.Errorf("type %s missing from l1/l1.go; have %v", name, keys(mod.Types))
			continue
		}
		if ty.Kind != kind {
			t.Errorf("type %s: kind=%q, want %q", name, ty.Kind, kind)
		}
	}
}

// functionByName finds a package-level function node by name.
func functionByName(t *testing.T, mod v2.Module, name string) v2.Callable {
	t.Helper()
	for sig, c := range mod.Functions {
		if strings.HasSuffix(sig, "."+name) {
			return c
		}
	}
	t.Fatalf("function %q missing; have %v", name, keys(mod.Functions))
	return v2.Callable{}
}

func TestGateL1_DeferredCallNode(t *testing.T) {
	d := functionByName(t, mustModule(t, buildDataflowL1V2(t), "l1/l1.go"), "D")
	var deferred, goroutine int
	for _, n := range d.Body {
		if n.Kind != "call" {
			continue
		}
		if n.IsDeferred {
			deferred++
			if n.IsGoroutine {
				t.Error("deferred call must not be a goroutine")
			}
		}
		if n.IsGoroutine {
			goroutine++
		}
	}
	if deferred != 1 || goroutine != 1 {
		t.Errorf("D body: deferred=%d goroutine=%d, want 1 and 1 (body=%v)", deferred, goroutine, d.Body)
	}
}

func TestGateL1_CoPositionedCallsBothKept(t *testing.T) {
	c := functionByName(t, mustModule(t, buildDataflowL1V2(t), "l1/l1.go"), "Chain")
	if len(c.Body) != 2 {
		t.Fatalf("Chain body has %d nodes, want 2: %v", len(c.Body), c.Body)
	}
	var bare, second string
	for k := range c.Body {
		if strings.HasSuffix(k, "/2") {
			second = k
		} else {
			bare = k
		}
	}
	if bare == "" || second != bare+"/2" {
		t.Fatalf("want keys L:C and L:C/2, got %v", keys(c.Body))
	}
	// The bare key is the outermost call (s.A().B()), which spans further.
	if c.Body[bare].Span.End[1] <= c.Body[second].Span.End[1] {
		t.Errorf("bare key should hold the outermost (longer) call: %+v vs %+v", c.Body[bare].Span, c.Body[second].Span)
	}
}

// buildDataflowL2V2 emits the L2 payload (resolver run) for the dataflow fixture.
func buildDataflowL2V2(t *testing.T) *v2.Analysis {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	dir, err := filepath.Abs(filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "testdata", "dataflow"))
	if err != nil {
		t.Fatalf("resolving fixture dir: %v", err)
	}
	b := syntactic_analysis.NewSymbolTableBuilder(dir)
	st, err := b.Build(nil, true)
	if err != nil {
		t.Fatalf("symbol table build failed: %v", err)
	}
	cg := semantic_analysis.NewCallGraphBuilder(dir, b.Fset(), b.Pkgs())
	edges := cg.Build(st)
	app := &schema.GoApplication{SymbolTable: st, CallGraph: edges}
	return Emit(app, "dataflow", dir, 2, "gate-test", 0, nil)
}

// TestGateL2_CoPositionedCallsResolveSeparately: in s.A().B() the outer call
// (bare key) resolves to B and the inner s.A() (key /2) resolves to A; the call
// graph has exactly the edges Chain->A and Chain->B.
func TestGateL2_CoPositionedCallsResolveSeparately(t *testing.T) {
	out := buildDataflowL2V2(t)
	mod := mustModule(t, out, "l1/l1.go")
	chain := functionByName(t, mod, "Chain")
	s := mod.Types["S"]
	var aID, bID string
	for sig, m := range s.Callables {
		switch {
		case strings.HasSuffix(sig, ".A"):
			aID = m.ID
		case strings.HasSuffix(sig, ".B"):
			bID = m.ID
		}
	}
	if aID == "" || bID == "" {
		t.Fatalf("methods A/B not found on S: %v", keys(s.Callables))
	}
	var bare, second string
	for k := range chain.Body {
		if strings.HasSuffix(k, "/2") {
			second = k
		} else {
			bare = k
		}
	}
	got := func(k string) string {
		if c := chain.Body[k].Callee; c != nil {
			return *c
		}
		return "<nil>"
	}
	if got(bare) != bID {
		t.Errorf("outer call %s callee=%s, want B %s", bare, got(bare), bID)
	}
	if got(second) != aID {
		t.Errorf("inner call %s callee=%s, want A %s", second, got(second), aID)
	}
	var dsts []string
	for _, e := range out.Application.CallGraph {
		if e.Src == chain.ID {
			dsts = append(dsts, e.Dst)
		}
	}
	sort.Strings(dsts)
	want := []string{aID, bID}
	sort.Strings(want)
	if strings.Join(dsts, ",") != strings.Join(want, ",") {
		t.Errorf("Chain edges = %v, want {A, B} = %v", dsts, want)
	}
}

// TestGateL1_BodyNodeIDs (R15): from L1 on, every body node carries its global
// id, v2.GlobalOrdinal(callable id, local key) — Python parity (its
// stamp_body_ids runs at L1) — and no parent (an L3 refinement).
func TestGateL1_BodyNodeIDs(t *testing.T) {
	out := buildDataflowL1V2(t)
	checked := 0
	var walk func(c v2.Callable)
	walk = func(c v2.Callable) {
		for k, n := range c.Body {
			if want := v2.GlobalOrdinal(c.ID, k); n.ID != want {
				t.Errorf("%s body %s id = %q, want %q", c.ID, k, n.ID, want)
			}
			if n.Parent != "" {
				t.Errorf("%s body %s carries parent %q at L1", c.ID, k, n.Parent)
			}
			checked++
		}
		for _, in := range c.Callables {
			walk(in)
		}
	}
	for _, m := range out.Application.SymbolTable {
		for _, f := range m.Functions {
			walk(f)
		}
		for _, ty := range m.Types {
			for _, c := range ty.Callables {
				walk(c)
			}
		}
	}
	if checked < 30 {
		t.Errorf("only %d L1 body nodes checked", checked)
	}
}
