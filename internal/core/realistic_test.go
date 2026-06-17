package core_test

// Targeted tests for Go-specific schema fields that the greeter fixture does not exercise:
//   is_goroutine, return_types (multiple), is_exported=false, receiver_type/name,
//   is_variadic, is_embedded, multi-file package, cyclomatic_complexity, specific edges.

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/codellm-devkit/codeanalyzer-go/internal/core"
	"github.com/codellm-devkit/codeanalyzer-go/internal/options"
	"github.com/codellm-devkit/codeanalyzer-go/internal/schema"
)

func realisticDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot determine source file path")
	}
	root := filepath.Join(filepath.Dir(file), "..", "..")
	abs, err := filepath.Abs(filepath.Join(root, "testdata", "realistic"))
	if err != nil {
		t.Fatalf("resolving realistic fixture dir: %v", err)
	}
	return abs
}

func runRealistic(t *testing.T, level options.AnalysisLevel) *schema.GoApplication {
	t.Helper()
	dir := realisticDir(t)
	outDir := t.TempDir()
	opts := options.AnalysisOptions{
		InputPath: dir,
		OutputDir: outDir,
		Level:     level,
		SkipTests: true,
		CacheDir:  t.TempDir(),
	}
	app, err := core.New(opts).Analyze()
	if err != nil {
		t.Fatalf("Analyze() failed: %v", err)
	}
	return app
}

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
	app := runRealistic(t, options.LevelSymbolTable)
	_, hasServer := app.SymbolTable["server/server.go"]
	_, hasMiddleware := app.SymbolTable["server/middleware.go"]
	if !hasServer {
		t.Error("server/server.go missing from symbol table")
	}
	if !hasMiddleware {
		t.Error("server/middleware.go missing from symbol table")
	}
	// Tags must live in middleware.go, not server.go.
	mw := app.SymbolTable["server/middleware.go"]
	if findCallableByName(mw, "Tags") == nil {
		t.Error("Tags function not found in server/middleware.go")
	}
}

// ── Embedded struct field ─────────────────────────────────────────────────────

func TestRealistic_EmbeddedField(t *testing.T) {
	app := runRealistic(t, options.LevelSymbolTable)
	srv := app.SymbolTable["server/server.go"]
	server, ok := srv.Types["Server"]
	if !ok {
		t.Fatal("GoType 'Server' not found in server/server.go")
	}
	for _, f := range server.Fields {
		if f.IsEmbedded {
			return // pass
		}
	}
	t.Errorf("Server has no embedded field; fields: %+v", server.Fields)
}

// ── Multiple return types — (T, error) pattern ────────────────────────────────

func TestRealistic_MultipleReturnTypes(t *testing.T) {
	app := runRealistic(t, options.LevelSymbolTable)
	srv := app.SymbolTable["server/server.go"]
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
	app := runRealistic(t, options.LevelSymbolTable)
	srv := app.SymbolTable["server/server.go"]
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
	app := runRealistic(t, options.LevelSymbolTable)
	srv := app.SymbolTable["server/server.go"]
	shutdown := findCallableByName(srv, "shutdown")
	if shutdown == nil {
		t.Fatal("method 'shutdown' not found in server/server.go")
	}
	if shutdown.IsExported {
		t.Error("shutdown.is_exported should be false")
	}
}

func TestRealistic_UnexportedWorkerMethod(t *testing.T) {
	app := runRealistic(t, options.LevelSymbolTable)
	wkr := app.SymbolTable["worker/worker.go"]
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
	app := runRealistic(t, options.LevelSymbolTable)
	srv := app.SymbolTable["server/server.go"]
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
	// Pointer receiver — type should contain '*' or 'Server'.
	if !strings.Contains(addr.ReceiverType, "Server") {
		t.Errorf("Addr().receiver_type %q should reference Server", addr.ReceiverType)
	}
}

func TestRealistic_ValueReceiver(t *testing.T) {
	app := runRealistic(t, options.LevelSymbolTable)
	// Describe is defined in middleware.go but its receiver type (Server) lives in
	// server.go — the reconcileCrossFileMethods pass attaches it to server.go's type.
	srv := app.SymbolTable["server/server.go"]
	describe := findCallableByName(srv, "Describe")
	if describe == nil {
		t.Fatal("method 'Describe' not found attached to Server in server/server.go")
	}
	// Value receiver — ReceiverType should not contain '*'.
	if strings.Contains(describe.ReceiverType, "*") {
		t.Errorf("Describe().receiver_type %q should be a value receiver (no '*')", describe.ReceiverType)
	}
	// Path should still record the physical definition file.
	if !strings.Contains(describe.Path, "middleware.go") {
		t.Errorf("Describe().path %q should point to middleware.go", describe.Path)
	}
}

// ── Variadic parameters ───────────────────────────────────────────────────────

func TestRealistic_VariadicParamTags(t *testing.T) {
	app := runRealistic(t, options.LevelSymbolTable)
	mw := app.SymbolTable["server/middleware.go"]
	tags := findCallableByName(mw, "Tags")
	if tags == nil {
		t.Fatal("function 'Tags' not found in server/middleware.go")
	}
	for _, p := range tags.Parameters {
		if p.IsVariadic {
			return // pass
		}
	}
	t.Errorf("Tags() has no variadic parameter; params: %+v", tags.Parameters)
}

func TestRealistic_VariadicParamCombine(t *testing.T) {
	app := runRealistic(t, options.LevelSymbolTable)
	wkr := app.SymbolTable["worker/worker.go"]
	combine := findCallableByName(wkr, "Combine")
	if combine == nil {
		t.Fatal("function 'Combine' not found in worker/worker.go")
	}
	for _, p := range combine.Parameters {
		if p.IsVariadic {
			return // pass
		}
	}
	t.Errorf("Combine() has no variadic parameter; params: %+v", combine.Parameters)
}

// ── Goroutine call site ───────────────────────────────────────────────────────

func TestRealistic_GoroutineCallsite(t *testing.T) {
	app := runRealistic(t, options.LevelSymbolTable)
	wkr := app.SymbolTable["worker/worker.go"]
	run := findCallableByName(wkr, "Run")
	if run == nil {
		t.Fatal("method 'Run' not found in worker/worker.go")
	}
	for _, cs := range run.CallSites {
		if cs.IsGoroutine {
			return // pass
		}
	}
	t.Errorf("Run() has no goroutine call site; sites: %+v", run.CallSites)
}

// ── Cyclomatic complexity ─────────────────────────────────────────────────────

func TestRealistic_CyclomaticComplexity(t *testing.T) {
	app := runRealistic(t, options.LevelSymbolTable)
	wkr := app.SymbolTable["worker/worker.go"]
	execute := findCallableByName(wkr, "execute")
	if execute == nil {
		t.Fatal("method 'execute' not found in worker/worker.go")
	}
	// execute() has an `if err != nil` branch → CC >= 2.
	if execute.CyclomaticComplexity < 2 {
		t.Errorf("execute().cyclomatic_complexity should be >= 2; got %d", execute.CyclomaticComplexity)
	}
}

// ── Interface detection ───────────────────────────────────────────────────────

func TestRealistic_InterfaceType(t *testing.T) {
	app := runRealistic(t, options.LevelSymbolTable)
	wkr := app.SymbolTable["worker/worker.go"]
	proc, ok := wkr.Types["Processor"]
	if !ok {
		t.Fatal("GoType 'Processor' not found in worker/worker.go")
	}
	if !proc.IsInterface {
		t.Error("Processor.is_interface should be true")
	}
}

// ── Specific call-graph edge ──────────────────────────────────────────────────

func TestRealistic_SpecificCallEdge(t *testing.T) {
	app := runRealistic(t, options.LevelCallGraph)
	// main() calls server.New() — this is a cross-package project-internal edge.
	const wantTarget = "example.com/realistic/server.New"
	for _, e := range app.CallGraph {
		if e.Target == wantTarget {
			return // pass
		}
	}
	t.Errorf("call graph missing expected edge to %s; edges: %v", wantTarget, edgeTargets(app))
}

func TestRealistic_CrossPackageEdges(t *testing.T) {
	app := runRealistic(t, options.LevelCallGraph)
	// At least one edge must cross the main→server boundary and one main→worker boundary.
	var serverEdge, workerEdge bool
	for _, e := range app.CallGraph {
		if strings.Contains(e.Target, "realistic/server.") {
			serverEdge = true
		}
		if strings.Contains(e.Target, "realistic/worker.") {
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

// ── Helpers ───────────────────────────────────────────────────────────────────

func edgeTargets(app *schema.GoApplication) []string {
	out := make([]string, 0, len(app.CallGraph))
	for _, e := range app.CallGraph {
		out = append(out, e.Target)
	}
	return out
}
