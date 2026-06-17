package core_test

// Targeted tests for Go-specific schema fields that the greeter fixture does not exercise:
//   is_goroutine, return_types (multiple), is_exported=false, receiver_type/name,
//   is_variadic, is_embedded, multi-file package, cyclomatic_complexity, specific edges.

import (
	"strings"
	"testing"

	"github.com/codellm-devkit/codeanalyzer-go/internal/schema"
)

// findCallableByName searches all functions and methods in a GoFile by short name.
func findCallableByName(f schema.GoFile, name string) *schema.GoCallable {
	for _, c := range f.Functions {
		if c.Name == name {
			c := c
			return &c
		}
	}
	for _, gt := range f.Types {
		for _, m := range gt.Methods {
			if m.Name == name {
				m := m
				return &m
			}
		}
	}
	return nil
}

// ── Multi-file package ────────────────────────────────────────────────────────

func TestRealistic_MultiFilePkg(t *testing.T) {
	_, hasServer := sharedMultipackageL1.SymbolTable["server/server.go"]
	_, hasMiddleware := sharedMultipackageL1.SymbolTable["server/middleware.go"]
	if !hasServer {
		t.Error("server/server.go missing from symbol table")
	}
	if !hasMiddleware {
		t.Error("server/middleware.go missing from symbol table")
	}
	mw := sharedMultipackageL1.SymbolTable["server/middleware.go"]
	if findCallableByName(mw, "Tags") == nil {
		t.Error("Tags function not found in server/middleware.go")
	}
}

// ── Embedded struct field ─────────────────────────────────────────────────────

func TestRealistic_EmbeddedField(t *testing.T) {
	srv := sharedMultipackageL1.SymbolTable["server/server.go"]
	server, ok := srv.Types["Server"]
	if !ok {
		t.Fatal("GoType 'Server' not found in server/server.go")
	}
	for _, f := range server.Fields {
		if f.IsEmbedded {
			return
		}
	}
	t.Errorf("Server has no embedded field; fields: %+v", server.Fields)
}

// ── Multiple return types — (T, error) pattern ────────────────────────────────

func TestRealistic_MultipleReturnTypes(t *testing.T) {
	srv := sharedMultipackageL1.SymbolTable["server/server.go"]
	newFn := findCallableByName(srv, "New")
	if newFn == nil {
		t.Fatal("function 'New' not found in server/server.go")
	}
	if len(newFn.ReturnTypes) < 2 {
		t.Fatalf("New() should have >= 2 return types; got %v", newFn.ReturnTypes)
	}
	hasError := false
	for _, rt := range newFn.ReturnTypes {
		if rt == "error" {
			hasError = true
		}
	}
	if !hasError {
		t.Errorf("New() return_types should include 'error'; got %v", newFn.ReturnTypes)
	}
}

func TestRealistic_ValidateReturnTypes(t *testing.T) {
	srv := sharedMultipackageL1.SymbolTable["server/server.go"]
	validate := findCallableByName(srv, "Validate")
	if validate == nil {
		t.Fatal("method 'Validate' not found in server/server.go")
	}
	if len(validate.ReturnTypes) != 2 {
		t.Fatalf("Validate() should have 2 return types; got %v", validate.ReturnTypes)
	}
}

// ── Unexported callables ──────────────────────────────────────────────────────

func TestRealistic_UnexportedMethod(t *testing.T) {
	srv := sharedMultipackageL1.SymbolTable["server/server.go"]
	shutdown := findCallableByName(srv, "shutdown")
	if shutdown == nil {
		t.Fatal("method 'shutdown' not found in server/server.go")
	}
	if shutdown.IsExported {
		t.Error("shutdown.is_exported should be false")
	}
}

func TestRealistic_UnexportedWorkerMethod(t *testing.T) {
	wkr := sharedMultipackageL1.SymbolTable["worker/worker.go"]
	execute := findCallableByName(wkr, "execute")
	if execute == nil {
		t.Fatal("method 'execute' not found in worker/worker.go")
	}
	if execute.IsExported {
		t.Error("execute.is_exported should be false")
	}
}

// ── Receiver type / name ──────────────────────────────────────────────────────

func TestRealistic_ReceiverType(t *testing.T) {
	srv := sharedMultipackageL1.SymbolTable["server/server.go"]
	addr := findCallableByName(srv, "Addr")
	if addr == nil {
		t.Fatal("method 'Addr' not found in server/server.go")
	}
	if addr.ReceiverType == "" {
		t.Error("Addr().receiver_type should be non-empty")
	}
	if addr.ReceiverName == "" {
		t.Error("Addr().receiver_name should be non-empty")
	}
	if !strings.Contains(addr.ReceiverType, "Server") {
		t.Errorf("Addr().receiver_type %q should reference Server", addr.ReceiverType)
	}
}

func TestRealistic_ValueReceiver(t *testing.T) {
	srv := sharedMultipackageL1.SymbolTable["server/server.go"]
	describe := findCallableByName(srv, "Describe")
	if describe == nil {
		t.Fatal("method 'Describe' not found attached to Server in server/server.go")
	}
	if strings.Contains(describe.ReceiverType, "*") {
		t.Errorf("Describe().receiver_type %q should be a value receiver (no '*')", describe.ReceiverType)
	}
	if !strings.Contains(describe.Path, "middleware.go") {
		t.Errorf("Describe().path %q should point to middleware.go", describe.Path)
	}
}

// ── Variadic parameters ───────────────────────────────────────────────────────

func TestRealistic_VariadicParamTags(t *testing.T) {
	mw := sharedMultipackageL1.SymbolTable["server/middleware.go"]
	tags := findCallableByName(mw, "Tags")
	if tags == nil {
		t.Fatal("function 'Tags' not found in server/middleware.go")
	}
	for _, p := range tags.Parameters {
		if p.IsVariadic {
			return
		}
	}
	t.Errorf("Tags() has no variadic parameter; params: %+v", tags.Parameters)
}

func TestRealistic_VariadicParamCombine(t *testing.T) {
	wkr := sharedMultipackageL1.SymbolTable["worker/worker.go"]
	combine := findCallableByName(wkr, "Combine")
	if combine == nil {
		t.Fatal("function 'Combine' not found in worker/worker.go")
	}
	for _, p := range combine.Parameters {
		if p.IsVariadic {
			return
		}
	}
	t.Errorf("Combine() has no variadic parameter; params: %+v", combine.Parameters)
}

// ── Goroutine call site ───────────────────────────────────────────────────────

func TestRealistic_GoroutineCallsite(t *testing.T) {
	wkr := sharedMultipackageL1.SymbolTable["worker/worker.go"]
	run := findCallableByName(wkr, "Run")
	if run == nil {
		t.Fatal("method 'Run' not found in worker/worker.go")
	}
	for _, cs := range run.CallSites {
		if cs.IsGoroutine {
			return
		}
	}
	t.Errorf("Run() has no goroutine call site; sites: %+v", run.CallSites)
}

// ── Cyclomatic complexity ─────────────────────────────────────────────────────

func TestRealistic_CyclomaticComplexity(t *testing.T) {
	wkr := sharedMultipackageL1.SymbolTable["worker/worker.go"]
	execute := findCallableByName(wkr, "execute")
	if execute == nil {
		t.Fatal("method 'execute' not found in worker/worker.go")
	}
	if execute.CyclomaticComplexity < 2 {
		t.Errorf("execute().cyclomatic_complexity should be >= 2; got %d", execute.CyclomaticComplexity)
	}
}

// ── Interface detection ───────────────────────────────────────────────────────

func TestRealistic_InterfaceType(t *testing.T) {
	wkr := sharedMultipackageL1.SymbolTable["worker/worker.go"]
	proc, ok := wkr.Types["Processor"]
	if !ok {
		t.Fatal("GoType 'Processor' not found in worker/worker.go")
	}
	if !proc.IsInterface {
		t.Error("Processor.is_interface should be true")
	}
}

// ── Specific call-graph edges ─────────────────────────────────────────────────

func TestRealistic_SpecificCallEdge(t *testing.T) {
	const wantTarget = "example.com/multipackage/server.New"
	for _, e := range sharedMultipackageL2.CallGraph {
		if e.Target == wantTarget {
			return
		}
	}
	t.Errorf("call graph missing expected edge to %s; edges: %v", wantTarget, edgeTargets(sharedMultipackageL2))
}

func TestRealistic_CrossPackageEdges(t *testing.T) {
	var serverEdge, workerEdge bool
	for _, e := range sharedMultipackageL2.CallGraph {
		if strings.Contains(e.Target, "multipackage/server.") {
			serverEdge = true
		}
		if strings.Contains(e.Target, "multipackage/worker.") {
			workerEdge = true
		}
	}
	if !serverEdge {
		t.Error("no call-graph edge into the server package")
	}
	if !workerEdge {
		t.Error("no call-graph edge into the worker package")
	}
}

// ── H6: LocalVariables assertions ────────────────────────────────────────────

// worker.Combine has `out := Result{}` — a local variable with a known type.
func TestRealistic_LocalVariablesPresent(t *testing.T) {
	wkr := sharedMultipackageL1.SymbolTable["worker/worker.go"]
	combine := findCallableByName(wkr, "Combine")
	if combine == nil {
		t.Fatal("function 'Combine' not found in worker/worker.go")
	}
	if len(combine.LocalVariables) == 0 {
		t.Fatal("Combine() should have at least one local variable; got none")
	}
}

// worker.execute has `r, err := p.Process(t)` — two local variables.
func TestRealistic_LocalVariablesHaveType(t *testing.T) {
	wkr := sharedMultipackageL1.SymbolTable["worker/worker.go"]
	execute := findCallableByName(wkr, "execute")
	if execute == nil {
		t.Fatal("method 'execute' not found in worker/worker.go")
	}
	for _, v := range execute.LocalVariables {
		if v.Name == "err" {
			if v.Type == "" {
				t.Error("local variable 'err' should have a non-empty type")
			}
			if v.Scope != "function" {
				t.Errorf("local variable 'err' scope should be 'function'; got %q", v.Scope)
			}
			return
		}
	}
	t.Errorf("local variable 'err' not found in execute(); vars: %+v", execute.LocalVariables)
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func edgeTargets(app *schema.GoApplication) []string {
	out := make([]string, 0, len(app.CallGraph))
	for _, e := range app.CallGraph {
		out = append(out, e.Target)
	}
	return out
}
