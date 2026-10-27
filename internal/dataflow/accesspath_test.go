package dataflow_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"strings"
	"testing"

	"github.com/codellm-devkit/codeanalyzer-go/internal/dataflow"
	v2 "github.com/codellm-devkit/codeanalyzer-go/internal/schema/v2"
)

// ---- a hermetic two-package source for access-path and def/use tests ----

const duLibSrc = `package lib

var V struct{ F int }

var W int

const C = 1

func F() {}
`

const duSrc = `package p

import "example.com/lib"

var G int

type H struct{ i int }
type Gs struct {
	h H
	x int
}
type Fs struct{ g Gs }
type P struct{ f Fs }

type T struct {
	f int
	s []int
}

type E2 struct{ *T }

func (t *T) Ptr()  {}
func (t T) Val()   {}
func (t *T) M() *T { return t }

type Mu struct{}

func (m *Mu) Unlock() {}

func sink(a ...any)                                                              {}
func id(x int) int                                                               { return x }
func pid(p *T) *T                                                                { return p }
func mut(p *T, s []int, t T, m map[int]int, i int, fn func(), e any, ch chan int) {}
func mut2(s []int, x int)                                                        {}

func Exprs(p *P, m map[int]int, q *T, a []T, k, i int, x T, e E2) {
	_ = p.f.g.h.i
	_ = p.f.g.x
	_ = p.f.g
	_ = m[k]
	_ = *q
	_ = (*q).f
	_ = a[i].f
	_ = lib.V.F
	_ = lib.W
	_ = G
	_ = (x)
	_ = lib.C
	_ = lib.F
	_ = id(k)
	_ = 42
	_ = x.Val
	_ = e.f
	_ = **(&q)
}

func Assign(x T, m map[int]int, q *int, k, v int) {
	x.f = v
	m[k] = v
	*q = v
	x.f += 1
	k++
	m[k]++
	var z, w int = k, v
	_ = z + w
}

func Calls(req T, s []int, p *T, t T, m map[int]int) {
	mut(&req, s, t, m, 1, nil, p, nil)
	t.Ptr()
	p.Ptr()
	t.Val()
	n := len(s)
	s = append(s, n)
	copy(s, s)
	_ = T(t)
	r := pid(&req)
	_ = r
}

func Chans(ch chan int, out chan<- int, v int) {
	for x := range ch {
		out <- x
	}
	select {
	case y, ok := <-ch:
		_, _ = y, ok
	case out <- v:
	case <-ch:
	}
	sink(<-ch)
}

func Defers(mu *Mu, x int, s []int) (n int, err error) {
	defer mu.Unlock()
	defer func() {
		if x > 0 {
			err = nil
		}
	}()
	defer mut2(s, x)
	n = x
	return
}

func Goes(s []int, x int) {
	go mut2(s, id(x))
	go func() { sink(x) }()
}

func (r *T) Entry(a int, _ int, b []int) (n int, err error) {
	G = a
	_ = lib.V.F + lib.W
	h := func() int {
		k := 1
		return func() int { return a + k + r.f }()
	}
	_ = h
	return
}

func NamedRet(x int) (n int) {
	return x
}

func Mixed(a int) int {
	b, a := 1, 2
	return a + b
}

func src() int { return 0 }

func ForPost(n int) int {
	x := 1
	for i2 := 0; i2 < 3; i2, x = i2+1, src() {
	}
	return x
}

func ForInit(n int) {
	for i, v := 0, 0; i < n; i++ {
		sink(v)
		v = src()
	}
}

func RangeAssign(a []int) int {
	last := 0
	for _, last = range a {
	}
	return last
}

type Hm map[string]string

func (h Hm) Set(k, v string) { h[k] = v }

type Writer interface{ Write(p []byte) }

func Recv(w Writer, h Hm, t T, data []byte) {
	w.Write(data)
	h.Set("a", "b")
	t.Val()
}

func IfShadow(x int) int {
	if x := id(x); x > 0 {
		return x
	}
	return x
}
`

type mapImporter map[string]*types.Package

func (m mapImporter) Import(path string) (*types.Package, error) { return m[path], nil }

type duFixture struct {
	fset  *token.FileSet
	file  *ast.File
	info  *types.Info
	decls map[string]*ast.FuncDecl
}

func newInfo() *types.Info {
	return &types.Info{
		Types:      map[ast.Expr]types.TypeAndValue{},
		Defs:       map[*ast.Ident]types.Object{},
		Uses:       map[*ast.Ident]types.Object{},
		Implicits:  map[ast.Node]types.Object{},
		Selections: map[*ast.SelectorExpr]*types.Selection{},
		Scopes:     map[ast.Node]*types.Scope{},
	}
}

func loadDU(t *testing.T) *duFixture {
	t.Helper()
	fset := token.NewFileSet()
	lf, err := parser.ParseFile(fset, "lib.go", duLibSrc, 0)
	if err != nil {
		t.Fatal(err)
	}
	lib, err := (&types.Config{}).Check("example.com/lib", fset, []*ast.File{lf}, newInfo())
	if err != nil {
		t.Fatal(err)
	}
	pf, err := parser.ParseFile(fset, "p.go", duSrc, 0)
	if err != nil {
		t.Fatal(err)
	}
	info := newInfo()
	conf := &types.Config{Importer: mapImporter{"example.com/lib": lib}}
	if _, err := conf.Check("example.com/p", fset, []*ast.File{pf}, info); err != nil {
		t.Fatal(err)
	}
	fx := &duFixture{fset: fset, file: pf, info: info, decls: map[string]*ast.FuncDecl{}}
	for _, d := range pf.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok {
			fx.decls[fd.Name.Name] = fd
		}
	}
	return fx
}

// expr finds the first expression in fn whose source text is text.
func (fx *duFixture) expr(t *testing.T, fn, text string) ast.Expr {
	t.Helper()
	var found ast.Expr
	ast.Inspect(fx.decls[fn].Body, func(n ast.Node) bool {
		if e, ok := n.(ast.Expr); ok && found == nil && types.ExprString(e) == text {
			found = e
		}
		return found == nil
	})
	if found == nil {
		t.Fatalf("%s: no expression %q", fn, text)
	}
	return found
}

// keyAt is the local key of the first occurrence of sub in duSrc, plus
// delta bytes.
func (fx *duFixture) keyAt(t *testing.T, sub string, delta int) string {
	t.Helper()
	off := strings.Index(duSrc, sub)
	if off < 0 {
		t.Fatalf("no %q in duSrc", sub)
	}
	p := fx.fset.Position(fx.fset.File(fx.file.Pos()).Pos(off + delta))
	return v2.LocalID(p.Line, p.Column)
}

func TestPathOf_KLimit(t *testing.T) {
	fx := loadDU(t)
	pObj := fx.info.Defs[fx.decls["Exprs"].Type.Params.List[0].Names[0]]
	cases := []struct {
		text string
		k    int
		want string
	}{
		{"p.f.g.h.i", 3, "p.f.g.*"},
		{"p.f.g.h.i", 4, "p.f.g.h.i"},
		{"p.f.g.h.i", 2, "p.f.*"},
		{"p.f.g.x", 3, "p.f.g.x"}, // exactly k steps: not truncated
		{"p.f.g", 1, "p.*"},
		{"p.f.g", 3, "p.f.g"},
	}
	for _, c := range cases {
		p, ok := dataflow.PathOf(fx.expr(t, "Exprs", c.text), fx.info, c.k)
		if !ok || p.Spell != c.want {
			t.Errorf("PathOf(%s, k=%d) = %q, %v; want %q", c.text, c.k, p.Spell, ok, c.want)
		}
		if p.Base != pObj {
			t.Errorf("PathOf(%s): Base = %v, want param p", c.text, p.Base)
		}
	}
}

func TestPathOf_IndexAndDeref(t *testing.T) {
	fx := loadDU(t)
	paths := []struct{ text, want string }{
		{"m[k]", "m[*]"},
		{"*q", "q.*"},
		{"(*q).f", "q.f"}, // spelled as Go's auto-deref q.f
		{"**(&q)", ""},    // &q is not a path
		{"a[i].f", "a[*].f"},
		{"lib.V.F", "example.com/lib::V.F"},
		{"lib.W", "example.com/lib::W"},
		{"G", "example.com/p::G"},
		{"(x)", "x"},
		{"e.f", "e.T.f"}, // the implicit embedded field is spelled out
	}
	for _, c := range paths {
		if c.want == "" {
			continue
		}
		p, ok := dataflow.PathOf(fx.expr(t, "Exprs", c.text), fx.info, 3)
		if !ok || p.Spell != c.want {
			t.Errorf("PathOf(%s) = %q, %v; want %q", c.text, p.Spell, ok, c.want)
		}
	}
	for _, text := range []string{"lib.C", "lib.F", "id(k)", "42", "x.Val", "**(&q)"} {
		if p, ok := dataflow.PathOf(fx.expr(t, "Exprs", text), fx.info, 3); ok {
			t.Errorf("PathOf(%s) = %q, want not a path", text, p.Spell)
		}
	}
	blank := &ast.Ident{Name: "_"}
	if _, ok := dataflow.PathOf(blank, fx.info, 3); ok {
		t.Errorf("PathOf(_) is a path")
	}
}

func TestPath_Covers(t *testing.T) {
	x := types.NewVar(token.NoPos, nil, "x", nil)
	x2 := types.NewVar(token.NoPos, nil, "x", nil) // a shadowing x
	m := types.NewVar(token.NoPos, nil, "m", nil)
	pkg := types.NewPackage("example.com/p", "p")
	g := types.NewVar(token.NoPos, pkg, "V", types.Typ[types.Int])
	pkg.Scope().Insert(g) // package-level: spelled pkgpath::V
	P := func(b types.Object, s string) dataflow.Path { return dataflow.Path{Base: b, Spell: s} }
	cases := []struct {
		p, q dataflow.Path
		want bool
	}{
		{P(x, "x"), P(x, "x"), true},            // exact
		{P(x, "x"), P(x, "x.f"), true},          // prefix
		{P(x, "x.f"), P(x, "x"), false},         // longer never covers shorter
		{P(x, "x.f"), P(x, "x.f.g"), true},      // prefix
		{P(x, "x.f"), P(x, "x.g"), false},       // sibling
		{P(x, "x.f"), P(x, "x.fg"), false},      // string prefix, not segment prefix
		{P(x, "x.f.*"), P(x, "x.f.g"), true},    // wildcard
		{P(x, "x.f.*"), P(x, "x.f.g.h"), true},  // wildcard, any remaining suffix
		{P(x, "x.f.*"), P(x, "x.f"), false},     // a wildcard stands for ≥1 step
		{P(x, "x.f"), P(x, "x.*"), false},       // kill direction: a field def does not kill x.*
		{P(x, "x.f.g"), P(x, "x.f.*"), false},   // kill direction: nor a deeper one x.f.*
		{P(x, "x.*"), P(x, "x[*]"), true},       // wildcard
		{P(x, "x.*"), P(x, "x[*].f.g"), true},   // wildcard
		{P(m, "m[*]"), P(m, "m[*].f"), true},    // prefix
		{P(m, "m[*]"), P(m, "m[*]"), true},      // [*] is every index
		{P(m, "m[*]"), P(m, "m.*"), true},       // a terminal [*] matches any remaining suffix
		{P(m, "m[*].f"), P(m, "m[*].g"), false}, // sibling under an element: a non-terminal [*] is a plain segment
		{P(m, "m[*].f"), P(m, "m[*].f.g"), true},
		{P(m, "m.f"), P(m, "m[*]"), false},
		{P(m, "m"), P(m, "m[*]"), true},                      // prefix
		{P(m, "m[*]"), P(m, "m"), false},                     // longer never covers shorter
		{P(x, "x"), P(x2, "x"), false},                       // shadowed: different base
		{P(x, "x.f"), P(x2, "x.f"), false},                   // shadowed: different base
		{P(nil, "<defer>:1:2"), P(nil, "<defer>:1:2"), true}, // pseudo base
		{P(nil, "<defer>:1:2"), P(nil, "<defer>:1:20"), false},
		{P(g, "example.com/p::V"), P(g, "example.com/p::V.f"), true},
		{P(g, "example.com/p::V.f"), P(g, "example.com/p::V.g"), false},
	}
	for _, c := range cases {
		if got := c.p.Covers(c.q); got != c.want {
			t.Errorf("%q.Covers(%q) = %v, want %v", c.p.Spell, c.q.Spell, got, c.want)
		}
	}
}
