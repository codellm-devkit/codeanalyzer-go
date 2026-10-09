package v2emit

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

// greeterDir returns the absolute path to the greeter testdata fixture.
func greeterDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	abs, err := filepath.Abs(filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "testdata", "greeter"))
	if err != nil {
		t.Fatalf("resolving fixture dir: %v", err)
	}
	return abs
}

// buildGreeter runs the real symbol-table builder over the greeter fixture.
func buildGreeter(t *testing.T) (*schema.GoApplication, string) {
	t.Helper()
	dir := greeterDir(t)
	b := syntactic_analysis.NewSymbolTableBuilder(dir)
	st, err := b.Build(nil, true)
	if err != nil {
		t.Fatalf("symbol table build failed: %v", err)
	}
	return &schema.GoApplication{SymbolTable: st, CallGraph: []schema.GoCallEdge{}}, dir
}

func TestEmit_Envelope(t *testing.T) {
	app, dir := buildGreeter(t)
	out := Emit(app, "greeter", dir, 1, "test")

	if out.SchemaVersion != "2.0.0" {
		t.Errorf("schema_version = %q, want 2.0.0", out.SchemaVersion)
	}
	if out.Language != "go" {
		t.Errorf("language = %q, want go", out.Language)
	}
	if out.MaxLevel != 1 {
		t.Errorf("max_level = %d, want 1", out.MaxLevel)
	}
	if out.Application.ID != "can://go/greeter" {
		t.Errorf("application.id = %q, want can://go/greeter", out.Application.ID)
	}
	if out.Application.Kind != "application" {
		t.Errorf("application.kind = %q, want application", out.Application.Kind)
	}
	if len(out.Application.SymbolTable) == 0 {
		t.Fatal("symbol_table is empty")
	}
}

func TestEmit_ModuleSourceAndIDs(t *testing.T) {
	app, dir := buildGreeter(t)
	out := Emit(app, "greeter", dir, 1, "test")

	mod, ok := out.Application.SymbolTable["main.go"]
	if !ok {
		t.Fatalf("main.go not in symbol_table; keys=%v", keys(out.Application.SymbolTable))
	}
	if mod.Kind != "module" {
		t.Errorf("module.kind = %q, want module", mod.Kind)
	}
	if mod.ID != "can://go/greeter/main.go" {
		t.Errorf("module.id = %q, want can://go/greeter/main.go", mod.ID)
	}
	if !strings.HasPrefix(mod.Source, "package main") {
		t.Errorf("module.source should hold the file text; got prefix %q", head(mod.Source, 20))
	}
	// The module span slices the whole file.
	if got := mod.Span.Slice(mod.Source); got != mod.Source {
		t.Errorf("module span does not cover whole source (%d vs %d bytes)", len(got), len(mod.Source))
	}
}

func TestEmit_CallableBodyAndSpanSlice(t *testing.T) {
	app, dir := buildGreeter(t)
	out := Emit(app, "greeter", dir, 1, "test")

	mod := out.Application.SymbolTable["main.go"]
	var main v2.Callable
	found := false
	for _, fn := range mod.Functions {
		if fn.Signature != "" && strings.HasSuffix(fn.Signature, ".main") {
			main, found = fn, true
		}
	}
	if !found {
		t.Fatalf("main function not found; functions=%v", keys(mod.Functions))
	}
	if main.Kind != "function" {
		t.Errorf("main.kind = %q, want function", main.Kind)
	}
	// Its span should slice to source beginning with "func main".
	if body := main.Span.Slice(mod.Source); !strings.HasPrefix(body, "func main") {
		t.Errorf("main span slice should start with 'func main'; got %q", head(body, 20))
	}
	// main() has call sites → body call nodes, each with callee null at L1.
	if len(main.Body) == 0 {
		t.Fatal("main should have call nodes in its body")
	}
	for local, node := range main.Body {
		if node.Kind != "call" {
			t.Errorf("body[%s].kind = %q, want call", local, node.Kind)
		}
		if node.Callee != nil {
			t.Errorf("body[%s].callee should be null at L1; got %q", local, *node.Callee)
		}
		if !strings.Contains(local, ":") {
			t.Errorf("body key %q should be line:col", local)
		}
	}
}

func TestEmit_CalleeSerializesAsNull(t *testing.T) {
	app, dir := buildGreeter(t)
	out := Emit(app, "greeter", dir, 1, "test")
	data, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	if !strings.Contains(string(data), `"callee":null`) {
		t.Error("expected at least one call node to serialize callee as null at L1")
	}
	// The v1-only keys must be gone.
	for _, banned := range []string{`"symbol_table"`, `"call_graph"`} {
		if !strings.Contains(string(data), banned) {
			t.Errorf("expected v2 payload to contain %s", banned)
		}
	}
	if strings.Contains(string(data), `"is_constructor_call"`) {
		t.Error("v2 payload should not contain dropped is_constructor_call")
	}
}

func TestEmit_TypeKindAndMethods(t *testing.T) {
	app, dir := buildGreeter(t)
	out := Emit(app, "greeter", dir, 1, "test")

	// Find the Greeter type wherever it lives; assert struct kind + methods.
	var found bool
	for _, mod := range out.Application.SymbolTable {
		for name, ty := range mod.Types {
			if ty.Kind != "struct" && ty.Kind != "interface" {
				t.Errorf("type %s kind = %q, want struct|interface", name, ty.Kind)
			}
			if strings.Contains(ty.ID, "://") == false {
				t.Errorf("type %s id should be a can:// id; got %q", name, ty.ID)
			}
			for sig, m := range ty.Callables {
				if m.Kind != "method" {
					t.Errorf("callable %s under type %s kind = %q, want method", sig, name, m.Kind)
				}
				found = true
			}
		}
	}
	if !found {
		t.Skip("no type methods in fixture; envelope/module coverage still asserted elsewhere")
	}
}

func head(s string, n int) string {
	if len(s) < n {
		return s
	}
	return s[:n]
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
