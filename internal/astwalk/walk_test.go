package astwalk_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/codellm-devkit/codeanalyzer-go/internal/astwalk"
)

// parseFlow parses testdata/dataflow/flow/flow.go and returns its functions by
// name.
func parseFlow(t *testing.T) (*token.FileSet, map[string]*ast.FuncDecl) {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	path := filepath.Join(filepath.Dir(thisFile), "..", "..", "testdata", "dataflow", "flow", "flow.go")
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	decls := map[string]*ast.FuncDecl{}
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil {
			decls[fd.Name.Name] = fd
		}
	}
	return fset, decls
}

// describe renders a call as its source text plus its go/defer flags.
func describe(fset *token.FileSet, src []byte, c astwalk.Call) string {
	tf := fset.File(c.Expr.Pos())
	s := string(src[tf.Offset(c.Expr.Pos()):tf.Offset(c.Expr.End())])
	if c.Goroutine {
		s += " [go]"
	}
	if c.Deferred {
		s += " [defer]"
	}
	return s
}

// TestWalk_CallsOrderMatchesL1: Calls lists a body's calls in L1 recording
// order — pre-order (an outer call before the calls nested in it, a go/defer
// statement's call before the calls in its operands), no function literal
// bodies — with the go/defer flag on the statement's own call only.
func TestWalk_CallsOrderMatchesL1(t *testing.T) {
	fset, decls := parseFlow(t)
	_, thisFile, _, _ := runtime.Caller(0)
	src := mustRead(t, filepath.Join(filepath.Dir(thisFile), "..", "..", "testdata", "dataflow", "flow", "flow.go"))
	for fn, want := range map[string][]string{
		"GoArgs":           {"f(g(x)) [go]", "g(x)", "func(y int) { _ = y }(h()) [go]", "h()"},
		"Chain":            {"s.A().B()", "s.A()"},
		"Nested":           {"f(g(y))", "g(y)"},
		"EarlyReturnDefer": {"cleanup() [defer]", "work(g(a)) [defer]", "g(a)", "cleanup()"},
		"Closure":          {"h()"},
	} {
		d, ok := decls[fn]
		if !ok {
			t.Fatalf("flow.go has no %s", fn)
		}
		var got []string
		for _, c := range astwalk.Calls(d.Body) {
			got = append(got, describe(fset, src, c))
		}
		if strings.Join(got, " | ") != strings.Join(want, " | ") {
			t.Errorf("%s: Calls = %q, want %q", fn, got, want)
		}
	}
	if got := astwalk.Calls(nil); len(got) != 0 {
		t.Errorf("Calls(nil) = %v, want none", got)
	}
}

const literalsSrc = `package p

func outer(g func(func())) {
	a := func() {
		_ = func() {} // nested: a's own literal, not outer's
	}
	g(func() {})
	defer func() {}()
	_ = a
}
`

// TestWalk_LiteralsNaming: Literals returns only the outermost function
// literals of a body, in ast.Inspect order, named closure_1, closure_2, …;
// a literal nested in another is the inner literal's own closure_1.
func TestWalk_LiteralsNaming(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "p.go", literalsSrc, 0)
	if err != nil {
		t.Fatal(err)
	}
	body := f.Decls[0].(*ast.FuncDecl).Body
	lits := astwalk.Literals(body)
	var got []string
	for _, l := range lits {
		got = append(got, l.Name+"@"+strings.TrimPrefix(fset.Position(l.Lit.Pos()).String(), "p.go:"))
	}
	if want := "closure_1@4:7,closure_2@7:4,closure_3@8:8"; strings.Join(got, ",") != want {
		t.Errorf("Literals = %v, want %s", got, want)
	}
	inner := astwalk.Literals(lits[0].Lit.Body)
	if len(inner) != 1 || inner[0].Name != "closure_1" || fset.Position(inner[0].Lit.Pos()).Line != 5 {
		t.Errorf("nested Literals = %+v, want one closure_1 on line 5", inner)
	}
	if got := astwalk.Literals(nil); len(got) != 0 {
		t.Errorf("Literals(nil) = %v, want none", got)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
