package core_test

// Tests for Go 1.18+ generic constructs — type parameters, union-constraint
// interfaces, multi-type-parameter functions, and methods on generic types.
// These exercise AST paths (IndexExpr receivers, TypeParams) that the greeter
// and realistic fixtures never reach.

import (
	"strings"
	"testing"
)

// ── Symbol table completeness ─────────────────────────────────────────────────

func TestGenerics_SymbolTableNonEmpty(t *testing.T) {
	if len(sharedGenericsL1.SymbolTable) == 0 {
		t.Fatal("generics symbol table is empty")
	}
}

func TestGenerics_PathKeysAreRelative(t *testing.T) {
	for key := range sharedGenericsL1.SymbolTable {
		if strings.HasPrefix(key, "/") {
			t.Errorf("symbol_table key is absolute: %s", key)
		}
	}
}

// ── Type name integrity ───────────────────────────────────────────────────────

func TestGenerics_SetTypePresentInSymbolTable(t *testing.T) {
	const wantFile = "set/set.go"
	f, ok := sharedGenericsL1.SymbolTable[wantFile]
	if !ok {
		t.Fatalf("%s not in symbol table; keys: %v", wantFile, keys(sharedGenericsL1.SymbolTable))
	}
	if _, ok := f.Types["Set"]; !ok {
		t.Errorf("GoType 'Set' not found in %s", wantFile)
	}
}

// The type name must be the base identifier only, not the parameterised form.
func TestGenerics_TypeNameHasNoTypeParams(t *testing.T) {
	for _, f := range sharedGenericsL1.SymbolTable {
		for name := range f.Types {
			if strings.ContainsAny(name, "[]") {
				t.Errorf("type name %q contains type-parameter brackets — should be stripped", name)
			}
		}
	}
}

// ── Methods on generic types ──────────────────────────────────────────────────

func TestGenerics_SetMethods(t *testing.T) {
	f := sharedGenericsL1.SymbolTable["set/set.go"]
	for _, want := range []string{"Add", "Remove", "Contains", "Len"} {
		t.Run(want, func(t *testing.T) {
			if findCallableByName(f, want) == nil {
				t.Errorf("method %q not found on Set", want)
			}
		})
	}
}

func TestGenerics_UnexportedMethodOnGenericType(t *testing.T) {
	f := sharedGenericsL1.SymbolTable["set/set.go"]
	snapshot := findCallableByName(f, "snapshot")
	if snapshot == nil {
		t.Fatal("unexported method 'snapshot' not found on Set")
	}
	if snapshot.IsExported {
		t.Error("snapshot.is_exported should be false")
	}
}

// ── Union-constraint interfaces ───────────────────────────────────────────────

func TestGenerics_OrderedIsInterface(t *testing.T) {
	f, ok := sharedGenericsL1.SymbolTable["fn/fn.go"]
	if !ok {
		t.Fatal("fn/fn.go not in symbol table")
	}
	ordered, ok := f.Types["Ordered"]
	if !ok {
		t.Fatal("GoType 'Ordered' not found in fn/fn.go")
	}
	if !ordered.IsInterface {
		t.Error("Ordered.is_interface should be true (union constraint)")
	}
}

func TestGenerics_NumericIsInterface(t *testing.T) {
	f := sharedGenericsL1.SymbolTable["fn/fn.go"]
	numeric, ok := f.Types["Numeric"]
	if !ok {
		t.Fatal("GoType 'Numeric' not found in fn/fn.go")
	}
	if !numeric.IsInterface {
		t.Error("Numeric.is_interface should be true")
	}
}

// ── Generic functions ─────────────────────────────────────────────────────────

func TestGenerics_SingleTypeParamFunctions(t *testing.T) {
	f := sharedGenericsL1.SymbolTable["fn/fn.go"]
	for _, name := range []string{"Min", "Max", "Filter"} {
		t.Run(name, func(t *testing.T) {
			if findCallableByName(f, name) == nil {
				t.Errorf("generic function %q not found in fn/fn.go", name)
			}
		})
	}
}

func TestGenerics_MapHasMultipleParams(t *testing.T) {
	f := sharedGenericsL1.SymbolTable["fn/fn.go"]
	mapFn := findCallableByName(f, "Map")
	if mapFn == nil {
		t.Fatal("generic function 'Map' not found in fn/fn.go")
	}
	// Map[T, U any](in []T, f func(T) U) []U — two declared parameters.
	if len(mapFn.Parameters) < 2 {
		t.Errorf("Map() should have >= 2 parameters; got %d", len(mapFn.Parameters))
	}
}
