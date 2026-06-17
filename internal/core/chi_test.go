package core_test

// Tests for the chi fixture — chi v5 (github.com/go-chi/chi/v5) analyzed as the
// project under test, not as a dependency.
//
// Goals:
//   1. Multi-package library (root + middleware) is fully indexed.
//   2. Interface types (chi.Router) and struct types (chi.Mux) are both captured.
//   3. Methods on *Mux (Get, Post, Route, …) appear in the symbol table.
//   4. Vendor files are absent (chi has no external deps; nothing to exclude).
//   5. Call graph edges (Level 2) are internally consistent — no dangling endpoints.

import (
	"strings"
	"testing"
)

// ── File coverage ─────────────────────────────────────────────────────────────

func TestChi_SymbolTableNonEmpty(t *testing.T) {
	if len(sharedChiL2.SymbolTable) == 0 {
		t.Fatal("chi symbol table is empty — analysis may have failed silently")
	}
}

// chi v5 has exactly 35 non-test Go source files (5 root + 30 middleware).
func TestChi_SymbolTableFileCount(t *testing.T) {
	const want = 35
	if got := len(sharedChiL2.SymbolTable); got != want {
		t.Errorf("symbol table: got %d file(s), want %d; keys: %v", got, want, keys(sharedChiL2.SymbolTable))
	}
}

func TestChi_PathKeysAreRelative(t *testing.T) {
	for key := range sharedChiL2.SymbolTable {
		if strings.HasPrefix(key, "/") {
			t.Errorf("symbol_table key is absolute: %s", key)
		}
	}
}

// ── Root package files ────────────────────────────────────────────────────────

func TestChi_RootFilesPresent(t *testing.T) {
	for _, name := range []string{"chi.go", "mux.go", "context.go", "chain.go", "tree.go"} {
		t.Run(name, func(t *testing.T) {
			if _, ok := sharedChiL2.SymbolTable[name]; !ok {
				t.Errorf("%s not in symbol table; keys: %v", name, keys(sharedChiL2.SymbolTable))
			}
		})
	}
}

// ── Middleware package files ──────────────────────────────────────────────────

func TestChi_MiddlewareFilesPresent(t *testing.T) {
	for _, name := range []string{
		"middleware/logger.go",
		"middleware/recoverer.go",
		"middleware/middleware.go",
	} {
		t.Run(name, func(t *testing.T) {
			if _, ok := sharedChiL2.SymbolTable[name]; !ok {
				t.Errorf("%s not in symbol table", name)
			}
		})
	}
}

// ── Interface and struct types ────────────────────────────────────────────────

// chi.go declares the Router interface.
func TestChi_RouterIsInterface(t *testing.T) {
	f, ok := sharedChiL2.SymbolTable["chi.go"]
	if !ok {
		t.Fatal("chi.go not in symbol table")
	}
	router, ok := f.Types["Router"]
	if !ok {
		t.Fatal("Router type not found in chi.go")
	}
	if !router.IsInterface {
		t.Error("Router should be an interface, got is_interface=false")
	}
}

// mux.go declares the Mux struct (not an interface).
func TestChi_MuxIsStruct(t *testing.T) {
	f, ok := sharedChiL2.SymbolTable["mux.go"]
	if !ok {
		t.Fatal("mux.go not in symbol table")
	}
	mux, ok := f.Types["Mux"]
	if !ok {
		t.Fatal("Mux type not found in mux.go")
	}
	if mux.IsInterface {
		t.Error("Mux should be a struct, got is_interface=true")
	}
}

// ── Methods on *Mux ───────────────────────────────────────────────────────────

func TestChi_MuxHasRoutingMethods(t *testing.T) {
	f, ok := sharedChiL2.SymbolTable["mux.go"]
	if !ok {
		t.Fatal("mux.go not in symbol table")
	}
	for _, method := range []string{"Get", "Post", "Put", "Delete", "Route", "Use", "With"} {
		t.Run(method, func(t *testing.T) {
			if findCallableByName(f, method) == nil {
				t.Errorf("method %q not found on Mux in mux.go", method)
			}
		})
	}
}

// ── Call graph: no dangling edges ─────────────────────────────────────────────

func TestChi_NoDanglingEdges(t *testing.T) {
	sigs := allSignatures(sharedChiL2)
	for _, e := range sharedChiL2.CallGraph {
		if !sigs[e.Source] {
			t.Errorf("dangling edge source: %s", e.Source)
		}
		if !sigs[e.Target] {
			t.Errorf("dangling edge target: %s", e.Target)
		}
	}
}

// ── H1: InnerCallables populated for functions with closures ──────────────────

// middleware/logger.go RequestLogger returns a closure-based middleware; its
// outer function body should have at least one inner callable after the fix.
func TestChi_RequestLoggerHasInnerCallables(t *testing.T) {
	f, ok := sharedChiL2.SymbolTable["middleware/logger.go"]
	if !ok {
		t.Fatal("middleware/logger.go not in symbol table")
	}
	rl := findCallableByName(f, "RequestLogger")
	if rl == nil {
		t.Fatal("RequestLogger not found in middleware/logger.go")
	}
	if len(rl.InnerCallables) == 0 {
		t.Error("RequestLogger should have at least one inner callable (closure), got none")
	}
}

// ── H2: IsConstructorCall for type-conversion call sites ──────────────────────

// tree.go RegisterMethod contains `mt := methodTyp(2 << n)`.
// methodTyp is a named type, so the call is a type-conversion (constructor) call.
func TestChi_RegisterMethodHasConstructorCallSite(t *testing.T) {
	f, ok := sharedChiL2.SymbolTable["tree.go"]
	if !ok {
		t.Fatal("tree.go not in symbol table")
	}
	rm := findCallableByName(f, "RegisterMethod")
	if rm == nil {
		t.Fatal("RegisterMethod not found in tree.go")
	}
	for _, site := range rm.CallSites {
		if site.IsConstructorCall {
			return // found
		}
	}
	t.Error("RegisterMethod should have at least one IsConstructorCall=true site (methodTyp(...))")
}

// ── H7: init() functions captured ────────────────────────────────────────────

// middleware/terminal.go, middleware/logger.go, and middleware/request_id.go
// each declare an init() function that should appear in the symbol table.
func TestChi_InitFunctionsPresent(t *testing.T) {
	for _, file := range []string{
		"middleware/terminal.go",
		"middleware/logger.go",
		"middleware/request_id.go",
	} {
		t.Run(file, func(t *testing.T) {
			f, ok := sharedChiL2.SymbolTable[file]
			if !ok {
				t.Fatalf("%s not in symbol table", file)
			}
			initFn := findCallableByName(f, "init")
			if initFn == nil {
				t.Errorf("init() not found in %s", file)
				return
			}
			if initFn.IsExported {
				t.Errorf("init() in %s should not be exported", file)
			}
		})
	}
}
