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
	"strings"
	"testing"

	"github.com/codellm-devkit/codeanalyzer-go/internal/schema"
	v2 "github.com/codellm-devkit/codeanalyzer-go/internal/schema/v2"
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
	return Emit(app, "multipackage", dir, 1, "gate-test")
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
		wantID := "can://go/multipackage/" + rel
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
	if out.Application.ID != "can://go/multipackage" {
		t.Errorf("application.id = %q, want can://go/multipackage", out.Application.ID)
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
	out := Emit(app, "multipackage", dir, 1, "gate-test")

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
