package dataflow_test

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/codellm-devkit/codeanalyzer-go/internal/dataflow"
	"github.com/codellm-devkit/codeanalyzer-go/internal/schema"
	"github.com/codellm-devkit/codeanalyzer-go/internal/semantic_analysis"
	"github.com/codellm-devkit/codeanalyzer-go/internal/syntactic_analysis"
	"github.com/codellm-devkit/codeanalyzer-go/internal/syntactic_analysis/v2emit"
	"github.com/codellm-devkit/codeanalyzer-go/internal/utils"
)

// TestRun_JobsDeterminism locks spec §4.12: the L3 output of testdata/chi is
// byte-identical across -j 1 and -j 8, and across two -j 8 runs.
func TestRun_JobsDeterminism(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	dir, err := filepath.Abs(filepath.Join(filepath.Dir(thisFile), "..", "..", "testdata", "chi"))
	if err != nil {
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
	units := b.L3Units(st)
	if len(units) < 100 {
		t.Fatalf("chi yields only %d L3 units", len(units))
	}

	emit := func(jobs int) []byte {
		l3 := dataflow.Run(b.Fset(), units, 3, jobs)
		if len(l3) != len(units) {
			t.Errorf("-j %d: %d results for %d units", jobs, len(l3), len(units))
		}
		data, err := json.Marshal(v2emit.Emit(app, "chi", dir, 3, "det", 3, l3))
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	one, eight, again := emit(1), emit(8), emit(8)
	if !bytes.Equal(one, eight) {
		t.Error("-j 1 and -j 8 outputs differ")
	}
	if !bytes.Equal(eight, again) {
		t.Error("two -j 8 runs differ")
	}
	if !bytes.Contains(one, []byte(`"cfg":[`)) || !bytes.Contains(one, []byte(`"ddg":[`)) {
		t.Error("L3 output carries no cfg/ddg edge lists")
	}
}

// lineDirectiveSrc maps its body to a different (virtual) file and line with
// a //line directive. Positions are real file positions (PositionFor(p,
// false)), so the call keys and spans name the real lines 7–11 of p.go, not
// the virtual 900+, and calls pair with their keys by Lparen (#26).
const lineDirectiveSrc = `package p

func f(x int) int { return x }

func g(x int) int {
//line virtual.go:900:1
	y := f(f(x))
	if y > 0 {
		f(y)
	}
	return y
}
`

func parseCheck(t *testing.T, src string) (*token.FileSet, *ast.File, *types.Info) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "p.go", src, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	info := &types.Info{
		Types: map[ast.Expr]types.TypeAndValue{}, Defs: map[*ast.Ident]types.Object{},
		Uses: map[*ast.Ident]types.Object{}, Implicits: map[ast.Node]types.Object{},
		Selections: map[*ast.SelectorExpr]*types.Selection{}, Scopes: map[ast.Node]*types.Scope{},
	}
	if _, err := (&types.Config{Importer: importer.Default()}).Check("p", fset, []*ast.File{f}, info); err != nil {
		t.Fatal(err)
	}
	return fset, f, info
}

func funcDecl(t *testing.T, f *ast.File, name string) *ast.FuncDecl {
	t.Helper()
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == name {
			return fd
		}
	}
	t.Fatalf("no func %s", name)
	return nil
}

// TestRun_LineDirective: under a //line directive the call keys and spans
// are real positions, not the directive's virtual ones, and every call key
// lands on its call node.
func TestRun_LineDirective(t *testing.T) {
	fset, f, info := parseCheck(t, lineDirectiveSrc)
	g := funcDecl(t, f, "g")
	calls := gateCallKeys(fset, g.Body)
	var keys []string
	for _, c := range calls {
		keys = append(keys, c.Key)
	}
	if strings.Join(keys, ",") != "7:7,7:9,9:3" {
		t.Fatalf("call keys = %v, want the real 7:7, 7:9, 9:3", keys)
	}
	key := dataflow.L3Key{Path: "p.go", Signature: "p.g"}
	out := dataflow.Run(fset, []dataflow.Unit{{Key: key, Fn: g, Info: info, Calls: calls}}, 3, 1)
	res, ok := out[key]
	if !ok {
		t.Fatal("no result for g")
	}
	byKey := map[string]dataflow.Node{}
	for _, n := range res.Nodes {
		byKey[n.Key] = n
	}
	for _, k := range keys {
		if n, ok := byKey[k]; !ok || n.Kind != dataflow.KindCall {
			t.Errorf("call key %s has no call node", k)
		}
	}
	if n := byKey["8:2"]; n.Kind != dataflow.KindBranch || n.StartLC != [2]int{8, 2} || n.EndLC != [2]int{10, 3} {
		t.Errorf("if node = %+v, want branch spanning 8:2–10:3", n)
	}
}

// TestRun_UnpairedCallSkips: a unit with a body call that no L1 key names
// (by Lparen) gets no result (never a silently mis-keyed one); other units
// are unaffected.
func TestRun_UnpairedCallSkips(t *testing.T) {
	fset, f, info := parseCheck(t, lineDirectiveSrc)
	g, fn := funcDecl(t, f, "g"), funcDecl(t, f, "f")
	bad := dataflow.L3Key{Path: "p.go", Signature: "p.g"}
	good := dataflow.L3Key{Path: "p.go", Signature: "p.f"}
	var out map[dataflow.L3Key]dataflow.Result
	stderr := captureStderr(t, func() {
		out = dataflow.Run(fset, []dataflow.Unit{
			{Key: bad, Fn: g, Info: info, Calls: gateCallKeys(fset, g.Body)[:1]}, // 1 key, 3 calls
			{Key: good, Fn: fn, Info: info, Calls: nil},
		}, 3, 2)
	})
	if _, ok := out[bad]; ok {
		t.Error("unit with unpaired calls produced a result")
	}
	if r, ok := out[good]; !ok || len(r.Cfg) == 0 {
		t.Errorf("good unit result = %+v, %v", r, ok)
	}
	if !strings.Contains(stderr, "L3 skipped for p.g (p.go): 2 calls in the body have no L1 call key") {
		t.Errorf("want the unpaired-call warning; got:\n%s", stderr)
	}
}

// TestRun_CallCountMismatchWarnsOnly: the L1-key count is a cross-check, not
// the pairing: a surplus key (here one naming no call of the body) warns, and
// the unit is still built with every call keyed by its Lparen.
func TestRun_CallCountMismatchWarnsOnly(t *testing.T) {
	fset, f, info := parseCheck(t, lineDirectiveSrc)
	g := funcDecl(t, f, "g")
	key := dataflow.L3Key{Path: "p.go", Signature: "p.g"}
	calls := append(gateCallKeys(fset, g.Body), dataflow.UnitCall{Key: "99:1", Lparen: token.NoPos})
	var out map[dataflow.L3Key]dataflow.Result
	stderr := captureStderr(t, func() {
		out = dataflow.Run(fset, []dataflow.Unit{{Key: key, Fn: g, Info: info, Calls: calls}}, 3, 1)
	})
	if !strings.Contains(stderr, "4 L1 call keys for 3 calls in the body") {
		t.Errorf("want the count cross-check warning; got:\n%s", stderr)
	}
	res, ok := out[key]
	if !ok {
		t.Fatal("count mismatch alone skipped the unit")
	}
	byKey := map[string]dataflow.Node{}
	for _, n := range res.Nodes {
		byKey[n.Key] = n
	}
	for _, c := range calls[:3] {
		if n, ok := byKey[c.Key]; !ok || n.Kind != dataflow.KindCall {
			t.Errorf("call key %s has no call node", c.Key)
		}
	}
	if _, ok := byKey["99:1"]; ok {
		t.Error("surplus key produced a node")
	}
}

// TestRun_ResultContract: on the flow fixture every result has @entry and
// @exit, a non-empty CFG and edges only between its nodes; DDG edges never
// touch a satellite (R13).
func TestRun_ResultContract(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(thisFile), "..", "..", "testdata", "dataflow")
	b := syntactic_analysis.NewSymbolTableBuilder(dir)
	st, err := b.Build(nil, true)
	if err != nil {
		t.Fatal(err)
	}
	units := b.L3Units(st)
	out := dataflow.Run(b.Fset(), units, 3, 4)
	if len(out) != len(units) || len(out) < 50 {
		t.Fatalf("%d results for %d units", len(out), len(units))
	}
	closure := dataflow.L3Key{Path: filepath.Join("flow", "flow.go"), Signature: "example.com/dataflow/flow.Closure.closure_1"}
	if _, ok := out[closure]; !ok {
		t.Errorf("no result for %v", closure)
	}
	for key, r := range out {
		nodes := map[string]dataflow.Node{}
		for _, n := range r.Nodes {
			nodes[n.Key] = n
		}
		if _, ok := nodes[dataflow.EntryKey]; !ok {
			t.Errorf("%v: no @entry", key)
		}
		if _, ok := nodes[dataflow.ExitKey]; !ok {
			t.Errorf("%v: no @exit", key)
		}
		if len(r.Cfg) == 0 {
			t.Errorf("%v: empty cfg", key)
		}
		for _, e := range r.Cfg {
			if _, ok := nodes[e.Src]; !ok {
				t.Errorf("%v: cfg src %s dangling", key, e.Src)
			}
			if _, ok := nodes[e.Dst]; !ok {
				t.Errorf("%v: cfg dst %s dangling", key, e.Dst)
			}
		}
		for _, e := range r.Ddg {
			if nodes[e.Src].Parent != "" || nodes[e.Dst].Parent != "" {
				t.Errorf("%v: ddg edge %v touches a satellite", key, e)
			}
		}
		for _, n := range r.Nodes {
			if n.Kind != dataflow.KindEntry && n.Kind != dataflow.KindExit && n.StartLC[0] == 0 {
				t.Errorf("%v: node %s has no position", key, n.Key)
			}
		}
	}
}

// TestRun_PanicInUnitIsContained: a unit whose body makes a builder panic
// (a switch body holding a non-case statement, which BuildNodes asserts
// against) gets no result; the run completes and the other units are built.
func TestRun_PanicInUnitIsContained(t *testing.T) {
	fset, f, info := parseCheck(t, lineDirectiveSrc)
	bad := &ast.FuncDecl{
		Name: ast.NewIdent("bad"),
		Type: &ast.FuncType{Params: &ast.FieldList{}},
		Body: &ast.BlockStmt{List: []ast.Stmt{
			&ast.SwitchStmt{Body: &ast.BlockStmt{List: []ast.Stmt{&ast.ExprStmt{X: ast.NewIdent("x")}}}},
		}},
	}
	badKey := dataflow.L3Key{Path: "p.go", Signature: "p.bad"}
	goodF := dataflow.L3Key{Path: "p.go", Signature: "p.f"}
	goodG := dataflow.L3Key{Path: "p.go", Signature: "p.g"}
	g := funcDecl(t, f, "g")
	for _, jobs := range []int{1, 4} {
		out := dataflow.Run(fset, []dataflow.Unit{
			{Key: goodF, Fn: funcDecl(t, f, "f"), Info: info},
			{Key: badKey, Fn: bad, Info: info},
			{Key: goodG, Fn: g, Info: info, Calls: gateCallKeys(fset, g.Body)},
		}, 3, jobs)
		if _, ok := out[badKey]; ok {
			t.Errorf("-j %d: panicking unit produced a result", jobs)
		}
		for _, k := range []dataflow.L3Key{goodF, goodG} {
			if r, ok := out[k]; !ok || len(r.Cfg) == 0 {
				t.Errorf("-j %d: %v result = %+v, %v; want a CFG", jobs, k, r, ok)
			}
		}
	}

	// The warning is always printed; at -vv the panic's stack follows it, so
	// the failing builder can be found.
	run := func() {
		dataflow.Run(fset, []dataflow.Unit{{Key: badKey, Fn: bad, Info: info}}, 3, 1)
	}
	if quiet := captureStderr(t, run); !strings.Contains(quiet, "L3 skipped for p.bad") || strings.Contains(quiet, "goroutine ") {
		t.Errorf("default verbosity: want the warning and no stack; got:\n%s", quiet)
	}
	utils.SetVerbosity(2)
	defer utils.SetVerbosity(0)
	if loud := captureStderr(t, run); !strings.Contains(loud, "L3 skipped for p.bad") ||
		!strings.Contains(loud, "DEBUG") || !strings.Contains(loud, "dataflow.BuildNodes") {
		t.Errorf("-vv: want the warning and a stack naming the panicking builder; got:\n%s", loud)
	}
}

// TestRun_TypeErrorsKeepDefs (I2): in a package that does not type-check
// (testdata/typeerr imports a missing package), go/types records no field
// selection on a value of the unresolved type and no object for an undeclared
// name. The field write must still be a def that reaches the later read.
func TestRun_TypeErrorsKeepDefs(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(thisFile), "..", "..", "testdata", "typeerr")
	b := syntactic_analysis.NewSymbolTableBuilder(dir)
	st, err := b.Build(nil, true)
	if err != nil {
		t.Fatal(err)
	}
	out := dataflow.Run(b.Fset(), b.L3Units(st), 3, 1)
	for _, c := range []struct{ fn, def, use, v string }{
		{"Forward", "11:2", "12:2", "req.Body"}, // unresolved type, resolved base
		{"Undeclared", "18:2", "19:2", "undeclared.Body"},
		{"Qualified", "24:2", "25:2", "example.com/typeerr/missing::Cfg.Body"},
		{"Asserted", "30:2", "31:2", "x"}, // x.(*missing.Req).Body = src weak-defines x.*
	} {
		key := dataflow.L3Key{Path: "typeerr.go", Signature: "example.com/typeerr." + c.fn}
		r, ok := out[key]
		if !ok {
			t.Errorf("no result for %v", key)
			continue
		}
		found := false
		for _, e := range r.Ddg {
			found = found || (e.Src == c.def && e.Dst == c.use && e.Var == c.v)
		}
		if !found {
			t.Errorf("%s: no ddg %s→%s on %q; have %v", c.fn, c.def, c.use, c.v, r.Ddg)
		}
	}
}

// captureStderr returns what fn writes to os.Stderr.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	done := make(chan string)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()
	defer func() { os.Stderr = old }()
	fn()
	_ = w.Close()
	os.Stderr = old
	return <-done
}

// TestRun_FlowFixtureHasNoCallCountMismatch: every L1 call site of every
// flow-fixture body pairs with its call by Lparen — including calls in a `go`
// statement's arguments — so no unit warns or is skipped.
func TestRun_FlowFixtureHasNoCallCountMismatch(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(thisFile), "..", "..", "testdata", "dataflow")
	b := syntactic_analysis.NewSymbolTableBuilder(dir)
	st, err := b.Build(nil, true)
	if err != nil {
		t.Fatal(err)
	}
	units := b.L3Units(st)
	var out map[dataflow.L3Key]dataflow.Result
	stderr := captureStderr(t, func() { out = dataflow.Run(b.Fset(), units, 3, 4) })
	if strings.Contains(stderr, "L1 call keys for") || strings.Contains(stderr, "have no L1 call key") {
		t.Errorf("call pairing warnings:\n%s", stderr)
	}
	if len(out) != len(units) {
		t.Errorf("%d results for %d units", len(out), len(units))
	}
}

// TestResult_ReleaseDropsASTs: Release nils every Node.Stmt, and the -a 3
// JSON emitted after it is byte-identical to the JSON emitted before it (no
// emitter reads Stmt).
func TestResult_ReleaseDropsASTs(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	dir, err := filepath.Abs(filepath.Join(filepath.Dir(thisFile), "..", "..", "testdata", "dataflow"))
	if err != nil {
		t.Fatal(err)
	}
	b := syntactic_analysis.NewSymbolTableBuilder(dir)
	st, err := b.Build(nil, true)
	if err != nil {
		t.Fatal(err)
	}
	cg := semantic_analysis.NewCallGraphBuilder(dir, b.Fset(), b.Pkgs())
	edges := cg.Build(st)
	app := &schema.GoApplication{SymbolTable: st, CallGraph: edges, Externals: cg.Externals()}
	out := dataflow.Run(b.Fset(), b.L3Units(st), 3, 4)
	emit := func() []byte {
		data, err := json.Marshal(v2emit.Emit(app, "dataflow", dir, 3, "rel", 3, out))
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	before := emit()
	stmts := 0
	for _, r := range out {
		for _, n := range r.Nodes {
			if n.Stmt != nil {
				stmts++
			}
		}
		r.Release()
	}
	if stmts == 0 {
		t.Fatal("no node had a Stmt before Release")
	}
	for key, r := range out {
		for _, n := range r.Nodes {
			if n.Stmt != nil {
				t.Errorf("%v: node %s keeps its Stmt after Release", key, n.Key)
			}
		}
	}
	if !bytes.Equal(before, emit()) {
		t.Error("-a 3 JSON changed after Release")
	}
}
