package sdg

import (
	"go/ast"
	"go/types"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"

	"github.com/codellm-devkit/codeanalyzer-go/internal/dataflow"
	"github.com/codellm-devkit/codeanalyzer-go/internal/utils"
)

// joined builds SSA over in and the join, and returns them with the
// project-function set the join is measured on: every function of the
// program whose code is project code (projectFuncs' rule) — not only the VTA
// seeds, so wrappers built on demand and every test variant count too.
func joined(t *testing.T, in Input) (*ssa.Program, *join, map[*ssa.Function]bool) {
	t.Helper()
	prog, pkgs, deg := buildSSA(in)
	for _, d := range deg {
		t.Logf("degraded: %+v", d)
	}
	if len(pkgs) == 0 {
		t.Fatal("no SSA package built")
	}
	j := newJoin(in)
	project := map[string]bool{}
	for _, p := range pkgs {
		project[p.Pkg.Path()] = true
	}
	funcs := map[*ssa.Function]bool{}
	for fn := range ssautil.AllFunctions(prog) {
		if project[homePath(fn)] && !strings.HasPrefix(fn.Synthetic, "from type information") {
			funcs[fn] = true
		}
	}
	project2, _ := projectFuncs(prog, pkgs)
	for fn := range project2 {
		funcs[fn] = true
	}
	return prog, j, funcs
}

// describeUnmapped lists the functions and calls of funcs that join nothing.
func describeUnmapped(j *join, funcs map[*ssa.Function]bool) []string {
	var out []string
	for fn := range funcs {
		if fn.Synthetic == synthPackageInit {
			continue
		}
		_, kind := j.resolve(fn)
		if kind == joinNone {
			out = append(out, "func "+fn.String()+" ["+fn.Synthetic+"]")
			continue
		}
		if kind != joinBody {
			continue
		}
		forCalls(fn, func(c ssa.CallInstruction) {
			if c.Common().Pos().IsValid() {
				if _, _, ok := j.callSite(c); !ok {
					out = append(out, "call "+c.String()+" in "+fn.String())
				}
			}
		})
	}
	sort.Strings(out)
	return out
}

// droppedDecls returns the bodies of the project FuncDecls that have no L3
// unit: the declarations the symbol table dropped (cgo wrappers, #29, and
// methods whose receiver type it did not find — today also a method declared
// before its receiver type in the same file). Their SSA functions, and
// closures in them, cannot join: that is unmapped_ssa by design.
func droppedDecls(in Input) map[ast.Node]bool {
	units := map[ast.Node]bool{}
	for _, u := range in.Units {
		units[u.Fn] = true
	}
	out := map[ast.Node]bool{}
	for _, p := range projectPackages(in) {
		for _, f := range p.Syntax {
			tf := in.Fset.File(f.Pos())
			if tf == nil || !utils.IsWithin(in.ProjectDir, tf.Name()) {
				continue
			}
			for _, d := range f.Decls {
				if fd, ok := d.(*ast.FuncDecl); ok && fd.Body != nil && !units[fd] {
					out[fd] = true
				}
			}
		}
	}
	return out
}

// inDropped reports whether fn's code is (inside) a dropped declaration.
func inDropped(fn *ssa.Function, dropped map[ast.Node]bool) bool {
	for f := fn; f != nil; f = f.Parent() {
		if dropped[f.Syntax()] {
			return true
		}
	}
	return false
}

// TestJoin_EveryProjectFunctionMaps: on the fixtures every project SSA
// function joins a callable and every positioned call instruction in a
// body-joined function joins its call node, except the functions of
// declarations the symbol table dropped (chi has one:
// http2FancyWriter.Push is declared before its receiver type). Every unit is
// the image of at least one SSA function.
func TestJoin_EveryProjectFunctionMaps(t *testing.T) {
	wantDropped := map[string][]string{}
	for _, name := range []string{"dataflow", "chi", "generics", "rangefunc"} {
		t.Run(name, func(t *testing.T) {
			in := loadInput(t, name)
			_, j, funcs := joined(t, in)
			dropped := droppedDecls(in)
			var lost []string
			for fn := range funcs {
				if _, kind := j.resolve(fn); kind == joinNone && fn.Synthetic != synthPackageInit {
					if !inDropped(fn, dropped) {
						t.Errorf("%s [%s] joins nothing and is not a dropped declaration", fn, fn.Synthetic)
					}
					lost = append(lost, fn.String())
				}
			}
			sort.Strings(lost)
			if !slices.Equal(lost, wantDropped[name]) {
				t.Errorf("unmapped functions = %v, want %v", lost, wantDropped[name])
			}
			for _, b := range describeUnmapped(j, funcs) {
				if strings.HasPrefix(b, "call ") {
					t.Errorf("unmapped %s", b)
				}
			}
			c := j.coverage(funcs)
			if d := j.degraded(c); (d != nil) != (len(wantDropped[name]) > 0) {
				t.Errorf("degraded = %+v", d)
			}
			hit := map[dataflow.L3Key]bool{}
			for fn := range funcs {
				if k, kind := j.resolve(fn); kind == joinBody {
					hit[k] = true
				}
			}
			for _, u := range in.Units {
				if !hit[u.Key] {
					t.Errorf("unit %v is the image of no SSA function", u.Key)
				}
			}
			t.Logf("%s: %d funcs (%d by wrapper, %d unmapped, +%d package inits), %d positioned calls (%d unmapped, +%d synthetic), %d units",
				name, c.Funcs, c.Wrappers, c.Unmapped, c.Inits, c.Calls, c.UnmappedCalls, c.Synthetic, len(in.Units))
		})
	}
}

// fnByName returns the SSA function of package pkg named name (a member, or
// a method "T.M" of a named type, pointer receiver first).
func fnByName(t *testing.T, prog *ssa.Program, pkg, name string) *ssa.Function {
	t.Helper()
	p := prog.ImportedPackage(pkg)
	if p == nil {
		t.Fatalf("no SSA package %s", pkg)
	}
	if typ, meth, ok := strings.Cut(name, "."); ok {
		T := p.Type(typ)
		if T == nil {
			t.Fatalf("%s has no type %s", pkg, typ)
		}
		for _, recv := range []bool{true, false} {
			mset := prog.MethodSets.MethodSet(T.Type())
			if recv {
				mset = prog.MethodSets.MethodSet(ptrTo(T.Type()))
			}
			if sel := mset.Lookup(p.Pkg, meth); sel != nil {
				return prog.MethodValue(sel)
			}
		}
		t.Fatalf("%s.%s has no method %s", pkg, typ, meth)
	}
	fn := p.Func(name)
	if fn == nil {
		t.Fatalf("%s has no func %s", pkg, name)
	}
	return fn
}

// TestJoin_MultipleInitsAndLineDirectives (Review Focus 4): each init#N joins
// its own file's init by AST pointer, and the calls of each join that file's
// call keys; calls under a //line directive join by Lparen.
func TestJoin_MultipleInitsAndLineDirectives(t *testing.T) {
	in := loadInput(t, "dataflow")
	prog, j, _ := joined(t, in)

	const initSig = "example.com/dataflow/inits.init"
	files := map[string]string{} // init#N → file
	inits := prog.ImportedPackage("example.com/dataflow/inits")
	for _, name := range []string{"init#1", "init#2"} {
		fn := inits.Func(name)
		if fn == nil {
			t.Fatalf("no %s", name)
		}
		k, ok := j.fnUnit(fn)
		if !ok || k.Signature != initSig {
			t.Fatalf("%s joins %v, %v; want an %s unit", name, k, ok, initSig)
		}
		files[name] = k.Path
		calls := 0
		forCalls(fn, func(c ssa.CallInstruction) {
			if !c.Common().Pos().IsValid() {
				return
			}
			u, key, ok := j.callSite(c)
			if !ok || u != k {
				t.Errorf("%s: call %s joins (%v, %q, %v), want its own unit %v", name, c, u, key, ok, k)
			}
			calls++
		})
		if calls != 2 {
			t.Errorf("%s: %d positioned calls, want 2 (setup, os.Getenv)", name, calls)
		}
	}
	if files["init#1"] == files["init#2"] {
		t.Errorf("both inits join %s", files["init#1"])
	}
	want := []string{filepath.Join("inits", "a.go"), filepath.Join("inits", "b.go")}
	got := []string{files["init#1"], files["init#2"]}
	sort.Strings(got)
	if !slices.Equal(got, want) {
		t.Errorf("init files = %v, want %v", got, want)
	}
	// The synthetic package initializer calls each init#N with no position.
	if _, ok := j.fnUnit(inits.Func("init")); ok {
		t.Error("the package initializer joins a callable")
	}

	caller := fnByName(t, prog, "example.com/dataflow/linedir", "Caller")
	k, ok := j.fnUnit(caller)
	if !ok || k.Signature != "example.com/dataflow/linedir.Caller" {
		t.Fatalf("linedir.Caller joins %v, %v", k, ok)
	}
	u := j.units[k]
	wantKeys := map[string]bool{}
	for _, c := range u.Calls {
		wantKeys[c.Key] = true
	}
	gotKeys := map[string]bool{}
	forCalls(caller, func(c ssa.CallInstruction) {
		if _, key, ok := j.callSite(c); ok {
			gotKeys[key] = true
		}
	})
	if len(wantKeys) != 3 || len(gotKeys) != len(wantKeys) {
		t.Errorf("linedir.Caller call keys = %v, want every L1 key %v", gotKeys, wantKeys)
	}
	// The real line of os.Getenv(helper()) is 13: keys are real positions,
	// and a virtual virtual.y:900 position would have joined nothing.
	for key := range gotKeys {
		if strings.HasPrefix(key, "900:") {
			t.Errorf("call key %s is a //line virtual position", key)
		}
	}
}

// TestJoin_ExternalTestPackage (Review Focus 5). With tests loaded, the
// external test package xtest_test and the test variant "xtest [xtest.test]"
// are SSA packages too; package user imports the plain xtest, so the program
// holds two SSA packages with path xtest, sharing a.go's syntax. Every
// project function of both still joins (variant-safe by AST pointer), the
// test files' functions join the test files' callables, and the plain and
// the variant Add join the same callable. With --skip-tests the test
// functions are absent and nothing dangles.
func TestJoin_ExternalTestPackage(t *testing.T) {
	const xt = "example.com/dataflow/xtest"
	aGo, testGo := filepath.Join("xtest", "a.go"), filepath.Join("xtest", "a_test.go")
	for _, skip := range []bool{false, true} {
		t.Run(map[bool]string{false: "tests", true: "skip-tests"}[skip], func(t *testing.T) {
			in := loadInputOpts(t, "dataflow", skip)
			prog, j, funcs := joined(t, in)
			if bad := describeUnmapped(j, funcs); len(bad) > 0 {
				t.Errorf("%d unmapped:\n  %s", len(bad), strings.Join(bad, "\n  "))
			}

			// Every SSA (*Counter).Add, of every variant: with tests, the
			// test variant "xtest [xtest.test]" and the plain xtest that
			// user imports are both roots built from a.go's shared syntax
			// (F2: the plain one's bodies carry the types user sees). Both
			// join the one callable by syntax.
			wantAdd := dataflow.L3Key{Path: aGo, Signature: xt + ".Counter.Add"}
			kinds := map[joinKind]int{}
			for fn := range ssautil.AllFunctions(prog) {
				if fn.Name() == "Add" && fn.Signature.Recv() != nil && homePath(fn) == xt && !strings.HasPrefix(fn.Synthetic, "wrapper") {
					k, kind := j.resolve(fn)
					if k != wantAdd {
						t.Errorf("%s [%s] (pkg %p) joins %v, %v; want %v", fn, fn.Synthetic, fn.Pkg, k, kind, wantAdd)
					}
					kinds[kind]++
				}
			}
			wantBodies := map[bool]int{true: 1, false: 2}[skip]
			if kinds[joinBody] != wantBodies || len(kinds) != 1 {
				t.Errorf("(*Counter).Add joins %v, want %d SSA bodies and nothing else", kinds, wantBodies)
			}
			// user.Use calls the plain xtest's stubs; they still name the
			// in-project callables, never an external.
			use := fnByName(t, prog, xt+"/user", "Use")
			forCalls(use, func(c ssa.CallInstruction) {
				callee := c.Common().StaticCallee()
				if callee == nil {
					return
				}
				if _, ok := j.fnUnit(callee); !ok {
					t.Errorf("user.Use callee %s [%s] joins nothing", callee, callee.Synthetic)
				}
				if sig, ok := j.externalOf(callee); ok {
					t.Errorf("user.Use callee %s is external %s", callee, sig)
				}
			})

			var testFns []dataflow.L3Key
			for fn := range funcs {
				if k, kind := j.resolve(fn); kind == joinBody && k.Path == testGo {
					testFns = append(testFns, k)
				}
			}
			sortKeys(testFns)
			testFns = slices.Compact(testFns)
			if skip {
				if len(testFns) != 0 {
					t.Errorf("test callables joined with --skip-tests: %v", testFns)
				}
				if p := prog.ImportedPackage(xt + "_test"); p != nil {
					t.Errorf("xtest_test has an SSA package with --skip-tests")
				}
				return
			}
			want := []dataflow.L3Key{
				{Path: testGo, Signature: xt + "_test.TestAdd"},
				{Path: testGo, Signature: xt + "_test.TestAdd.closure_1"},
				{Path: testGo, Signature: xt + "_test.helper"},
			}
			if !slices.Equal(testFns, want) {
				t.Errorf("test callables joined = %v, want %v", testFns, want)
			}
			// helper's c.Add(2) joins its call key in a_test.go.
			helper := fnByName(t, prog, xt+"_test", "helper")
			n := 0
			forCalls(helper, func(c ssa.CallInstruction) {
				if u, key, ok := j.callSite(c); ok && u == want[2] && key != "" {
					n++
				}
			})
			if n != 1 {
				t.Errorf("helper: %d calls joined, want 1", n)
			}
		})
	}
}

// TestJoin_WrappersUnwrap: each SSA wrapper kind (§4.8 unwrap list) joins
// the method it wraps: $bound (method value), $thunk (method expression),
// the pointer wrapper of a value method, and a promoted method of an
// embedding struct. Their calls are synthetic: none joins a call node.
func TestJoin_WrappersUnwrap(t *testing.T) {
	in := loadInput(t, "dataflow")
	prog, j, funcs := joined(t, in)
	const xt = "example.com/dataflow/xtest"
	add := dataflow.L3Key{Path: filepath.Join("xtest", "a.go"), Signature: xt + ".Counter.Add"}
	get := dataflow.L3Key{Path: filepath.Join("xtest", "a.go"), Signature: xt + ".Counter.Get"}

	kinds := map[string]dataflow.L3Key{}
	for fn := range funcs {
		if homePath(fn) != xt || fn.Synthetic == "" {
			continue
		}
		// Keyed by fn.String(): an interface method's wrappers share the
		// concrete method's short names (Add$bound, Add$thunk).
		if (strings.HasPrefix(fn.Synthetic, "bound method wrapper") || strings.HasPrefix(fn.Synthetic, "thunk")) && !strings.Contains(fn.String(), "Adder") {
			kinds[fn.String()] = mustJoin(t, j, fn) // concrete-method wrappers; Adder's are checked below
		}
		if fn.Syntax() == nil && fn.Synthetic != synthPackageInit {
			if _, kind := j.resolve(fn); kind == joinWrapper {
				forCalls(fn, func(c ssa.CallInstruction) {
					if _, _, ok := j.callSite(c); ok {
						t.Errorf("wrapper %s: call %s joined a call node", fn, c)
					}
				})
			}
		}
	}
	for _, name := range []string{"(*" + xt + ".Counter).Add$bound", "(*" + xt + ".Counter).Add$thunk"} {
		if k, ok := kinds[name]; !ok || k != add {
			t.Errorf("%s joins %v (seen %v), want %v (wrappers seen: %v)", name, k, ok, add, kinds)
		}
	}
	// An interface method has a symbol-table key but no unit: its $bound and
	// $thunk are !ok for fnUnit and name it through ifaceMethod.
	iface := dataflow.L3Key{Path: filepath.Join("xtest", "a.go"), Signature: xt + ".Adder.Add"}
	if _, ok := j.units[iface]; ok {
		t.Fatalf("interface method %v has a unit", iface)
	}
	var ifaceWrappers int
	for fn := range funcs {
		if obj := fn.Object(); obj != nil && fn.Syntax() == nil && obj.Pkg() != nil && obj.Pkg().Path() == xt && obj.Name() == "Add" && types.IsInterface(fn.Object().Type().(*types.Signature).Recv().Type()) {
			if k, ok := j.fnUnit(fn); ok {
				t.Errorf("fnUnit(%s [%s]) = %v, true; want !ok (an interface method has no unit)", fn, fn.Synthetic, k)
			}
			if k, ok := j.ifaceMethod(fn); !ok || k != iface {
				t.Errorf("ifaceMethod(%s [%s]) = %v, %v; want %v, true", fn, fn.Synthetic, k, ok, iface)
			}
			if _, ok := j.bodilessDecl(fn); ok {
				t.Errorf("bodilessDecl(%s) is ok for an interface method", fn)
			}
			ifaceWrappers++
		}
	}
	if ifaceWrappers != 2 {
		t.Errorf("%d wrappers of the interface method Adder.Add, want 2 ($bound, $thunk)", ifaceWrappers)
	}
	// A concrete method and its wrappers still join their unit, and are not
	// interface methods.
	for _, fn := range []*ssa.Function{fnByName(t, prog, xt, "Counter.Add"), fnByName(t, prog, xt, "Outer.Add")} {
		if k, ok := j.fnUnit(fn); !ok || k != add || j.units[k].Fn == nil {
			t.Errorf("fnUnit(%s) = %v, %v; want the unit %v", fn, k, ok, add)
		}
		if _, ok := j.ifaceMethod(fn); ok {
			t.Errorf("ifaceMethod(%s) is ok for a concrete method", fn)
		}
	}
	if k := mustJoin(t, j, fnByName(t, prog, xt, "Counter.Get")); k != get {
		t.Errorf("(*Counter).Get (pointer wrapper) joins %v, want %v", k, get)
	}
	if k := mustJoin(t, j, fnByName(t, prog, xt, "Outer.Add")); k != add {
		t.Errorf("Outer.Add (promoted) joins %v, want %v", k, add)
	}
}

// TestJoin_BodilessDeclHasNoUnit: a function declared without a Go body
// (testdata/dataflow/asm, implemented in assembly) is in the symbol table
// but has no unit. Its SSA function is !ok for fnUnit and names it through
// bodilessDecl; it is not unmapped, and its caller's call still joins.
func TestJoin_BodilessDeclHasNoUnit(t *testing.T) {
	in := loadInput(t, "dataflow")
	prog, j, funcs := joined(t, in)
	const pkg = "example.com/dataflow/asm"
	want := dataflow.L3Key{Path: filepath.Join("asm", "asm.go"), Signature: pkg + ".Add"}
	if _, ok := j.units[want]; ok {
		t.Fatalf("bodiless %v has a unit", want)
	}
	add := fnByName(t, prog, pkg, "Add")
	if k, ok := j.fnUnit(add); ok {
		t.Errorf("fnUnit(asm.Add) = %v, true; want !ok", k)
	}
	if k, ok := j.bodilessDecl(add); !ok || k != want {
		t.Errorf("bodilessDecl(asm.Add) = %v, %v; want %v, true", k, ok, want)
	}
	if _, ok := j.ifaceMethod(add); ok {
		t.Error("ifaceMethod(asm.Add) is ok")
	}
	if c := j.coverage(funcs); c.NoUnit == 0 {
		t.Errorf("coverage %+v counts no function without a unit", c)
	}
	n := 0
	forCalls(fnByName(t, prog, pkg, "Use"), func(c ssa.CallInstruction) {
		if _, _, ok := j.callSite(c); ok {
			n++
		}
	})
	if n != 1 {
		t.Errorf("asm.Use: %d calls joined, want 1", n)
	}
}

func mustJoin(t *testing.T, j *join, fn *ssa.Function) dataflow.L3Key {
	t.Helper()
	k, ok := j.fnUnit(fn)
	if !ok {
		t.Errorf("%s [%s] joins nothing", fn, fn.Synthetic)
	}
	return k
}

// TestJoin_GenericInstancesJoinOrigin: with InstantiateGenerics, every
// instance of a generic function or method joins its one origin callable,
// and its calls join the origin's call keys.
func TestJoin_GenericInstancesJoinOrigin(t *testing.T) {
	in := loadInput(t, "generics")
	_, j, funcs := joined(t, in)
	byOrigin := map[dataflow.L3Key]int{}
	for fn := range funcs {
		if !strings.HasPrefix(fn.Synthetic, "instance of") {
			continue
		}
		k, kind := j.resolve(fn)
		if kind != joinBody {
			t.Errorf("instance %s joins %v, %v", fn, k, kind)
			continue
		}
		byOrigin[k]++
	}
	add := dataflow.L3Key{Path: filepath.Join("set", "set.go"), Signature: "example.com/generics/set.Set.Add"}
	if byOrigin[add] == 0 {
		t.Errorf("no instance of Set.Add joins %v (instances: %v)", add, byOrigin)
	}

	// Twice[T] calls Ident[T] through an instantiation wrapper (its type
	// arguments are parameterized): it joins Ident as a wrapper.
	in = loadInput(t, "dataflow")
	_, j, funcs = joined(t, in)
	ident := dataflow.L3Key{Path: filepath.Join("generic", "b.go"), Signature: "example.com/dataflow/generic.Ident"}
	wrappers := 0
	for fn := range funcs {
		if strings.HasPrefix(fn.Synthetic, synthInstWrapper) {
			k, kind := j.resolve(fn)
			if k != ident || kind != joinWrapper {
				t.Errorf("%s joins %v, %v; want %v as a wrapper", fn, k, kind, ident)
			}
			wrappers++
		}
	}
	if wrappers == 0 {
		t.Error("no instantiation wrapper in testdata/dataflow/generic")
	}
}

// TestJoin_RangeFuncYieldFolds: a range-over-func yield function folds into
// its enclosing callable, and the call in the loop body joins that
// callable's call key.
func TestJoin_RangeFuncYieldFolds(t *testing.T) {
	in := loadInput(t, "rangefunc")
	prog, j, _ := joined(t, in)
	outer := fnByName(t, prog, "example.com/rangefunc", "RangeFuncCall")
	want, ok := j.fnUnit(outer)
	if !ok {
		t.Fatal("RangeFuncCall joins nothing")
	}
	var yield *ssa.Function
	for _, a := range outer.AnonFuncs {
		if a.Synthetic == synthYield {
			yield = a
		}
	}
	if yield == nil {
		t.Fatal("RangeFuncCall has no yield function")
	}
	if k, ok := j.fnUnit(yield); !ok || k != want {
		t.Errorf("yield joins %v, %v; want %v", k, ok, want)
	}
	n := 0
	forCalls(yield, func(c ssa.CallInstruction) {
		if u, _, ok := j.callSite(c); ok && u == want {
			n++
		}
	})
	if n != 1 {
		t.Errorf("yield: %d calls joined (sink(v)), want 1", n)
	}
}

// TestJoin_UnmappedDegrades: a function literal in a package-level variable
// initializer has no callable, so its SSA function (a child of the package
// initializer) is unmapped_ssa, counted, with its package listed.
func TestJoin_UnmappedDegrades(t *testing.T) {
	in := loadInput(t, "unmapped")
	_, j, funcs := joined(t, in)
	c := j.coverage(funcs)
	if c.Unmapped != 1 {
		t.Errorf("coverage = %+v, want 1 unmapped function", c)
	}
	d := j.degraded(c)
	if d == nil || d.Code != CodeUnmappedSSA || !slices.Equal(d.Packages, []string{"example.com/unmapped"}) {
		t.Fatalf("degraded = %+v, want unmapped_ssa [example.com/unmapped]", d)
	}
	if strings.Contains(d.Detail, "#29") {
		t.Errorf("detail %q names #29 for a package with no cgo", d.Detail)
	}
	res := Run(in)
	if !hasCode(res.Degraded, CodeUnmappedSSA) {
		t.Errorf("Run degraded = %+v, want unmapped_ssa", res.Degraded)
	}
}

// TestJoin_CgoNamesIssue29: a cgo package's functions have no callable
// (#29), so they are unmapped_ssa and the detail names the issue.
func TestJoin_CgoNamesIssue29(t *testing.T) {
	in := loadInput(t, "cgo")
	res := Run(in)
	var d *Degraded
	for i := range res.Degraded {
		if res.Degraded[i].Code == CodeUnmappedSSA {
			d = &res.Degraded[i]
		}
	}
	if d == nil {
		t.Skipf("no unmapped_ssa (cgo disabled?): %+v", res.Degraded)
	}
	if !strings.Contains(d.Detail, "#29") || !slices.Equal(d.Packages, []string{"example.com/cgo/c"}) {
		t.Errorf("unmapped_ssa = %+v, want package example.com/cgo/c and a #29 note", d)
	}
}

func hasCode(deg []Degraded, code string) bool {
	for _, d := range deg {
		if d.Code == code {
			return true
		}
	}
	return false
}

// TestSDG_L3SkippedDegrades (A16): a unit with no L3 result is listed by key
// under l3_skipped.
func TestSDG_L3SkippedDegrades(t *testing.T) {
	in := loadInput(t, "dataflow")
	if hasCode(Run(in).Degraded, CodeL3Skipped) {
		t.Fatal("clean fixture has l3_skipped")
	}
	drop := []dataflow.L3Key{in.Units[3].Key, in.Units[0].Key}
	l3 := map[dataflow.L3Key]dataflow.Result{}
	for k, r := range in.L3 {
		l3[k] = r
	}
	for _, k := range drop {
		delete(l3, k)
	}
	in.L3 = l3
	res := Run(in)
	var got *Degraded
	for i := range res.Degraded {
		if res.Degraded[i].Code == CodeL3Skipped {
			got = &res.Degraded[i]
		}
	}
	if got == nil {
		t.Fatalf("degraded = %+v, want l3_skipped", res.Degraded)
	}
	want := []dataflow.L3Key{in.Units[0].Key, in.Units[3].Key} // units are sorted by key
	if !slices.Equal(got.Callables, want) {
		t.Errorf("l3_skipped callables = %v, want %v", got.Callables, want)
	}
	// A skipped unit still joins: its calls fall back to its L1 call keys.
	if hasCode(res.Degraded, CodeUnmappedSSA) {
		t.Errorf("dropping L3 results unmapped SSA: %+v", res.Degraded)
	}
}

// TestSDG_IllTypedPackageDegrades (Review Focus 1, F4): testdata/typeerr
// does not type-check: partial_types lists it (its go/packages errors), it
// gets no SSA (partial_ssa, then no_ssa), nothing panics, and its L3
// results are those of -a 3.
func TestSDG_IllTypedPackageDegrades(t *testing.T) {
	in := loadInput(t, "typeerr")
	plain := dataflow.Run(in.Fset, in.Units, 3, 1)
	res := Run(in)
	codes := codesOf(res.Degraded)
	if !slices.Equal(codes, []string{CodePartialTypes, CodePartialSSA, CodeNoSSA}) {
		t.Fatalf("codes = %v, want [partial_types partial_ssa no_ssa]", codes)
	}
	for _, d := range res.Degraded[:2] {
		if !slices.Equal(d.Packages, []string{"example.com/typeerr"}) {
			t.Errorf("%s packages = %v", d.Code, d.Packages)
		}
	}
	if len(in.L3) == 0 || len(in.L3) != len(plain) {
		t.Fatalf("L3 has %d results, -a 3 has %d", len(in.L3), len(plain))
	}
	for k, r := range plain {
		got := in.L3[k]
		if !slices.Equal(got.Cfg, r.Cfg) || !slices.Equal(got.Cdg, r.Cdg) || !slices.Equal(got.Ddg, r.Ddg) || len(got.Nodes) != len(r.Nodes) {
			t.Errorf("%v: L3 differs from -a 3", k)
		}
	}
}

func codesOf(deg []Degraded) []string {
	out := make([]string, len(deg))
	for i, d := range deg {
		out[i] = d.Code
	}
	return out
}

// TestSDG_RangeFuncDegrades (D8): a program that ranges over a function is
// degraded range_func, listing the callables (by key) and their package;
// the join itself is complete.
func TestSDG_RangeFuncDegrades(t *testing.T) {
	in := loadInput(t, "rangefunc")
	res := Run(in)
	if codes := codesOf(res.Degraded); !slices.Equal(codes, []string{CodeRangeFunc}) {
		t.Fatalf("codes = %v, want [range_func]", codes)
	}
	d := res.Degraded[0]
	want := []dataflow.L3Key{
		{Path: "r.go", Signature: "example.com/rangefunc.RangeFunc"},
		{Path: "r.go", Signature: "example.com/rangefunc.RangeFuncCall"},
	}
	if !slices.Equal(d.Callables, want) || !slices.Equal(d.Packages, []string{"example.com/rangefunc"}) {
		t.Errorf("range_func = %+v, want callables %v in example.com/rangefunc", d, want)
	}
	if hasCode(Run(loadInput(t, "dataflow")).Degraded, CodeRangeFunc) {
		t.Error("dataflow (go 1.21, no range-over-func) is range_func")
	}
}

// TestJoin_Deterministic: two independent loads and SSA builds give the same
// join of every project function, compared by (function string, key) — the
// join must not depend on SSA build or map order.
func TestJoin_Deterministic(t *testing.T) {
	render := func() []string {
		in := loadInput(t, "chi")
		_, j, funcs := joined(t, in)
		var out []string
		for fn := range funcs {
			k, kind := j.resolve(fn)
			line := fn.String() + " → " + k.Path + " " + k.Signature + " " + string(rune('0'+kind))
			forCalls(fn, func(c ssa.CallInstruction) {
				if _, key, ok := j.callSite(c); ok {
					line += " " + key
				}
			})
			out = append(out, line)
		}
		sort.Strings(out)
		return out
	}
	if a, b := render(), render(); !slices.Equal(a, b) {
		t.Error("two runs join differently")
	}
}

// TestJoin_ExternalOf: a library callee's signature is L2's: the call of
// os.Getenv in inits is os.Getenv, (*strings.Builder) methods are
// strings.Builder.<M>, and a project function is not external.
func TestJoin_ExternalOf(t *testing.T) {
	in := loadInput(t, "dataflow")
	prog, j, _ := joined(t, in)
	init1 := prog.ImportedPackage("example.com/dataflow/inits").Func("init#1")
	var got []string
	forCalls(init1, func(c ssa.CallInstruction) {
		if callee := c.Common().StaticCallee(); callee != nil {
			if sig, ok := j.externalOf(callee); ok {
				got = append(got, sig)
			} else if _, ok := j.fnUnit(callee); !ok {
				t.Errorf("callee %s is neither external nor in-project", callee)
			}
		}
	})
	if !slices.Equal(got, []string{"os.Getenv"}) {
		t.Errorf("externals of init#1 = %v, want [os.Getenv]", got)
	}
	if fn := prog.ImportedPackage("example.com/vlib"); fn != nil {
		if sig, ok := j.externalOf(fn.Func("F")); !ok || sig != "example.com/vlib.F" {
			t.Errorf("vendored vlib.F: externalOf = %q, %v; want example.com/vlib.F", sig, ok)
		}
	}
	if _, ok := j.externalOf(fnByName(t, prog, "example.com/dataflow/flow", "IfElse")); ok {
		t.Error("flow.IfElse is external")
	}
}

func ptrTo(t types.Type) types.Type { return types.NewPointer(t) }
