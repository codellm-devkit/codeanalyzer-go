package sdg

import (
	"go/types"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/tools/go/callgraph"
	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"

	"github.com/codellm-devkit/codeanalyzer-go/internal/dataflow"
	"github.com/codellm-devkit/codeanalyzer-go/internal/semantic_analysis"
	"github.com/codellm-devkit/codeanalyzer-go/internal/syntactic_analysis"
	"github.com/codellm-devkit/codeanalyzer-go/internal/utils"
)

// loadInput loads testdata/<name> the way the analyzer does at -a 4
// (symbol-table builder, tests skipped, the L2 call graph, L3 with side tables) and returns the
// sdg input over it.
func loadInput(t *testing.T, name string) Input {
	t.Helper()
	return loadInputOpts(t, name, true)
}

// loadInputOpts is loadInput with the --skip-tests choice.
func loadInputOpts(t *testing.T, name string, skipTests bool) Input {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	dir, err := filepath.Abs(filepath.Join(filepath.Dir(thisFile), "..", "..", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	b := syntactic_analysis.NewSymbolTableBuilder(dir)
	st, err := b.Build(nil, skipTests)
	if err != nil {
		t.Fatalf("symbol table build failed: %v", err)
	}
	cg := semantic_analysis.NewCallGraphBuilder(dir, b.Fset(), b.Pkgs())
	edges := cg.Build(st)
	units := b.L3Units(st)
	return Input{
		Fset: b.Fset(), Pkgs: b.Pkgs(), ProjectDir: dir, SymbolTable: st,
		Externals: cg.Externals(), CallGraph: edges,
		Units: units, L3: dataflow.RunWithSideTables(b.Fset(), units, 3, 1), K: 3, Jobs: 1,
	}
}

// dataflowPkgs are the project packages of testdata/dataflow (tests
// skipped), sorted.
var dataflowPkgs = []string{
	"example.com/dataflow/asm", "example.com/dataflow/flow", "example.com/dataflow/generic", "example.com/dataflow/httpsql", "example.com/dataflow/initclosure", "example.com/dataflow/inits",
	"example.com/dataflow/l1", "example.com/dataflow/linedir", "example.com/dataflow/sdg", "example.com/dataflow/vend",
	"example.com/dataflow/wrap", "example.com/dataflow/xfile", "example.com/dataflow/xtest", "example.com/dataflow/xtest/user",
}

// TestSSA_BuildsProjectPackagesOnly: bodies are built for the project's
// packages only. Every function with syntax lies in a project file, and no
// function declared outside the project (stdlib, the vendored vlib) has blocks.
func TestSSA_BuildsProjectPackagesOnly(t *testing.T) {
	in := loadInput(t, "dataflow")
	prog, pkgs, deg := buildSSA(in)
	if len(deg) != 0 {
		t.Fatalf("degraded = %+v, want none", deg)
	}
	var built []string
	for _, p := range pkgs {
		built = append(built, p.Pkg.Path())
	}
	want := dataflowPkgs
	if !slices.Equal(built, want) {
		t.Fatalf("built packages = %v, want %v", built, want)
	}

	project := map[string]bool{}
	for _, p := range want {
		project[p] = true
	}
	withBody := 0
	for fn := range ssautil.AllFunctions(prog) {
		if syn := fn.Syntax(); syn != nil {
			file := in.Fset.File(syn.Pos()).Name()
			if !utils.IsWithin(in.ProjectDir, file) || utils.IsVendored(utils.RelativePath(in.ProjectDir, file)) {
				t.Errorf("%s has syntax outside the project: %s", fn, file)
			}
		}
		// Wrappers ($bound, promotion, thunks) are synthesized with blocks
		// for whatever they wrap; only declared code and instances count.
		declared := fn.Synthetic == "" || strings.HasPrefix(fn.Synthetic, "instance of")
		if declared && len(fn.Blocks) > 0 {
			if !project[homePath(fn)] {
				t.Errorf("%s (package %q) has blocks; only project bodies are built", fn, homePath(fn))
			}
			withBody++
		}
	}
	if withBody == 0 {
		t.Fatal("no project function was built")
	}
	if fn := prog.ImportedPackage("example.com/dataflow/flow").Func("IfElse"); fn == nil || len(fn.Blocks) == 0 {
		t.Error("flow.IfElse has no SSA body")
	}
	osPkg := prog.ImportedPackage("os")
	if osPkg == nil {
		t.Fatal("dependency os has no SSA package")
	}
	if fn := osPkg.Func("Getenv"); fn == nil || len(fn.Blocks) != 0 {
		t.Errorf("os.Getenv = %v, want a bodiless function from types", fn)
	}
	vlib := prog.ImportedPackage("example.com/vlib")
	if vlib == nil {
		t.Fatal("vendored dependency vlib has no SSA package")
	}
	if fn := vlib.Func("F"); fn == nil || len(fn.Blocks) != 0 {
		t.Errorf("vlib.F = %v, want a bodiless function from types (vendored code is not the project)", fn)
	}
}

// TestSSA_PanicDegradesPackage: a panic while one package builds is
// recovered; that package is listed under partial_ssa and the rest still
// build. When every package panics, no_ssa is added.
func TestSSA_PanicDegradesPackage(t *testing.T) {
	in := loadInput(t, "dataflow")
	defer func() { buildHook = nil }()

	buildHook = func(p *ssa.Package) {
		if p.Pkg.Path() == "example.com/dataflow/flow" {
			panic("boom")
		}
	}
	prog, pkgs, deg := buildSSA(in)
	if len(deg) != 1 || deg[0].Code != "partial_ssa" || !slices.Equal(deg[0].Packages, []string{"example.com/dataflow/flow"}) {
		t.Fatalf("degraded = %+v, want one partial_ssa naming example.com/dataflow/flow", deg)
	}
	for _, p := range pkgs {
		if p.Pkg.Path() == "example.com/dataflow/flow" {
			t.Error("the panicked package is still returned as built")
		}
	}
	if len(pkgs) != len(dataflowPkgs)-1 {
		t.Errorf("built %d packages, want the other %d", len(pkgs), len(dataflowPkgs)-1)
	}
	if fn := prog.ImportedPackage("example.com/dataflow/l1"); fn == nil || len(fn.Members) == 0 {
		t.Error("l1 was not built after flow panicked")
	}

	buildHook = func(*ssa.Package) { panic("boom") }
	_, pkgs, deg = buildSSA(in)
	if len(pkgs) != 0 {
		t.Errorf("built %d packages, want 0", len(pkgs))
	}
	var codes []string
	for _, d := range deg {
		codes = append(codes, d.Code)
	}
	if !slices.Equal(codes, []string{"partial_ssa", "no_ssa"}) {
		t.Fatalf("codes = %v, want [partial_ssa no_ssa]", codes)
	}
	if !slices.Equal(deg[0].Packages, dataflowPkgs) {
		t.Errorf("partial_ssa lists %v, want all %d packages", deg[0].Packages, len(dataflowPkgs))
	}
	if res := Run(in); len(res.Degraded) != 2 || res.Degraded[1].Code != "no_ssa" {
		t.Errorf("Run degraded = %+v, want partial_ssa + no_ssa and no VTA", res.Degraded)
	}
}

// TestSSA_IllTypedPackageDegrades: a package with type errors gets no SSA and
// is listed under partial_ssa; it is the only package, so no_ssa follows.
func TestSSA_IllTypedPackageDegrades(t *testing.T) {
	in := loadInput(t, "typeerr")
	_, pkgs, deg := buildSSA(in)
	if len(pkgs) != 0 {
		t.Errorf("built %d packages from an ill-typed project", len(pkgs))
	}
	if len(deg) != 2 || deg[0].Code != "partial_ssa" || !slices.Equal(deg[0].Packages, []string{"example.com/typeerr"}) || deg[1].Code != "no_ssa" {
		t.Fatalf("degraded = %+v, want partial_ssa [example.com/typeerr] then no_ssa", deg)
	}
}

// TestRun_CleanFixtureNotDegraded: SSA and VTA over the fixture degrade
// nothing.
func TestRun_CleanFixtureNotDegraded(t *testing.T) {
	if res := Run(loadInput(t, "dataflow")); len(res.Degraded) != 0 {
		t.Fatalf("degraded = %+v, want none", res.Degraded)
	}
}

// TestRun_VTAPanicDegrades: a VTA panic is recovered as no_vta.
func TestRun_VTAPanicDegrades(t *testing.T) {
	in := loadInput(t, "dataflow")
	defer func(f func(map[*ssa.Function]bool, *callgraph.Graph) *callgraph.Graph) { vtaCallGraph = f }(vtaCallGraph)
	vtaCallGraph = func(map[*ssa.Function]bool, *callgraph.Graph) *callgraph.Graph { panic("vta boom") }
	res := Run(in)
	if len(res.Degraded) != 1 || res.Degraded[0].Code != "no_vta" {
		t.Fatalf("degraded = %+v, want one no_vta", res.Degraded)
	}
}

// TestProjectFuncs_CoversClosuresAndMethods: the VTA function set holds the
// project's functions, their closures and methods, and nothing declared
// outside the project.
func TestProjectFuncs_CoversClosuresAndMethods(t *testing.T) {
	in := loadInput(t, "dataflow")
	prog, pkgs, _ := buildSSA(in)
	funcs, _ := projectFuncs(prog, pkgs)
	project := map[string]bool{}
	for _, p := range pkgs {
		project[p.Pkg.Path()] = true
	}
	anon, methods := 0, 0
	for fn := range funcs {
		if !project[homePath(fn)] {
			t.Errorf("%s (package %q) is outside the project", fn, homePath(fn))
		}
		if fn.Parent() != nil {
			anon++
		}
		if fn.Signature.Recv() != nil {
			methods++
		}
	}
	if anon == 0 || methods == 0 {
		t.Errorf("funcs has %d closures and %d methods; want both > 0", anon, methods)
	}
	if !funcs[prog.ImportedPackage("example.com/dataflow/flow").Func("IfElse")] {
		t.Error("flow.IfElse is not in the VTA function set")
	}
}

// panicOn returns a callHook that panics on a call to the function named name.
func panicOn(name string) func(*types.Func) {
	return func(f *types.Func) {
		if f.Name() == name {
			panic("boom in " + name)
		}
	}
}

// withTimeout runs f, failing the test if it does not return within d (a
// deadlocked SSA program blocks forever; recover cannot catch that).
func withTimeout(t *testing.T, d time.Duration, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); f() }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("did not finish within %s: the SSA program deadlocked", d)
	}
}

// TestSSA_MidBuildPanicDoesNotDeadlock: testdata/ssapanic packages a and b
// share the instance g.Map[int]. Building a creates it under a's builder,
// then panics on the call to Boom, so a's builder never marks that instance
// done. Package b, built next in the same program, would wait on it forever.
// buildSSA must instead drop the program and rebuild without a: the run
// finishes, partial_ssa names a alone, and b keeps its SSA body.
func TestSSA_MidBuildPanicDoesNotDeadlock(t *testing.T) {
	in := loadInput(t, "ssapanic")
	callHook = panicOn("Boom")
	defer func() { callHook = nil }()

	var prog *ssa.Program
	var pkgs []*ssa.Package
	var deg []Degraded
	withTimeout(t, 30*time.Second, func() { prog, pkgs, deg = buildSSA(in) })

	if len(deg) != 1 || deg[0].Code != "partial_ssa" || !slices.Equal(deg[0].Packages, []string{"example.com/ssapanic/a"}) {
		t.Fatalf("degraded = %+v, want one partial_ssa naming example.com/ssapanic/a", deg)
	}
	var built []string
	for _, p := range pkgs {
		built = append(built, p.Pkg.Path())
	}
	if want := []string{"example.com/ssapanic/b", "example.com/ssapanic/g", "example.com/ssapanic/w"}; !slices.Equal(built, want) {
		t.Fatalf("built = %v, want %v", built, want)
	}
	useB := prog.ImportedPackage("example.com/ssapanic/b").Func("UseB")
	if useB == nil || len(useB.Blocks) == 0 {
		t.Fatal("b.UseB has no SSA body after a's build panicked")
	}
	if a := prog.ImportedPackage("example.com/ssapanic/a"); a != nil {
		t.Errorf("the rebuilt program still holds package a (%d members)", len(a.Members))
	}
	if res := Run(in); len(res.Degraded) != 1 || res.Degraded[0].Code != "partial_ssa" {
		t.Errorf("Run degraded = %+v, want only partial_ssa (VTA runs on the rebuilt program)", res.Degraded)
	}
}

// TestRun_FuncSetPanicDegradesToNoVTA: building the VTA function set builds
// method wrappers on demand ((*w.V).Never, which nothing calls, so the package
// build never made it). A panic there is recovered as no_vta.
func TestRun_FuncSetPanicDegradesToNoVTA(t *testing.T) {
	in := loadInput(t, "ssapanic")
	callHook = panicOn("Never")
	defer func() { callHook = nil }()

	var res Result
	withTimeout(t, 30*time.Second, func() { res = Run(in) })
	if len(res.Degraded) != 1 || res.Degraded[0].Code != "no_vta" {
		t.Fatalf("degraded = %+v, want one no_vta", res.Degraded)
	}
}
