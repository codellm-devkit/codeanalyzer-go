// Package metamorphic holds the metamorphic relations (MRs) over the L3/L4
// analysis: source transforms that must leave the analysis unchanged up to a
// known renaming (the exact oracle), or must keep every flow the original had
// (the one-way reachability oracle). Fixtures only catch an invariance or
// soundness bug when someone writes that exact case; the relations check it
// on any module, real apps included (spec §8, Metamorphic relations).
package metamorphic

import (
	"fmt"
	"io"
	"path/filepath"
	"sort"

	"github.com/codellm-devkit/codeanalyzer-go/internal/analysis"
	"github.com/codellm-devkit/codeanalyzer-go/internal/dataflow"
	"github.com/codellm-devkit/codeanalyzer-go/internal/schema"
	"github.com/codellm-devkit/codeanalyzer-go/internal/sdg"
	"github.com/codellm-devkit/codeanalyzer-go/internal/semantic_analysis"
	"github.com/codellm-devkit/codeanalyzer-go/internal/syntactic_analysis"
	"github.com/codellm-devkit/codeanalyzer-go/internal/syntactic_analysis/v2emit"
)

// AppName is the --app-name every run uses, so both sides of a relation
// mint the same can://<app> prefix (external ids included).
const AppName = "mr"

// Run is one in-process analysis: its normalized view and the load errors
// of the project packages (from the analyzer's own go/packages load).
type Run struct {
	View   *View
	Errors []string
}

// Analyze runs the analyzer pipeline over dir at level (3 or 4) the way
// core.Analyzer.Analyze does — symbol table, call graph, pass pipeline,
// dataflow, and at level 4 sdg.Run with its call-graph additions — minus
// `go mod download` (the inputs are already materialized), and emits the v2
// payload. targets is --target-files (absolute paths); nil is a full run.
// Test files are skipped (the CLI default).
func Analyze(dir string, level int, targets []string) (*Run, error) {
	b := syntactic_analysis.NewSymbolTableBuilder(dir)
	st, err := b.Build(targets, true)
	if err != nil {
		return nil, fmt.Errorf("symbol table: %w", err)
	}
	var errs []string
	for _, p := range b.Pkgs() {
		for _, e := range p.Errors {
			errs = append(errs, p.PkgPath+": "+e.Msg)
		}
	}
	sort.Strings(errs)
	cg := semantic_analysis.NewCallGraphBuilder(dir, b.Fset(), b.Pkgs())
	app := &schema.GoApplication{SymbolTable: st, CallGraph: cg.Build(st), Externals: cg.Externals(), Entrypoints: map[string][]schema.GoEntrypoint{}}
	app.Externals = cg.Externals()
	if err := analysis.RunPipeline(app, analysis.AnalysisContext{ProjectDir: dir}); err != nil {
		return nil, err
	}
	units := b.L3Units(st)
	const k, jobs = 3, 4
	var res map[dataflow.L3Key]dataflow.Result
	var l4 *sdg.Result
	if level >= 4 {
		res = dataflow.RunWithSideTables(b.Fset(), units, k, jobs)
		r := sdg.Run(sdg.Input{
			Fset: b.Fset(), Pkgs: b.Pkgs(), Variants: b.Loaded(), ProjectDir: dir, SymbolTable: st,
			Externals: app.Externals, CallGraph: app.CallGraph,
			Units: units, L3: res, K: k, Jobs: jobs,
		})
		r.ApplyTo(app)
		l4 = &r
	} else {
		res = dataflow.Run(b.Fset(), units, k, jobs)
	}
	dataflow.ReleaseAll(res)
	// The payload is streamed (EmitTo) into the view one module at a time,
	// so a large app's tree never exists in memory at once.
	pr, pw := io.Pipe()
	go func() {
		pw.CloseWithError(v2emit.EmitTo(pw, app, AppName, dir, level, "metamorphic", k, res, l4))
	}()
	v, err := Collect(pr)
	pr.CloseWithError(err)
	if err != nil {
		return nil, err
	}
	return &Run{View: v, Errors: errs}, nil
}

// realDir is dir with symlinks resolved: go/packages reports resolved file
// names, and the analyzer keys files relative to the input root, so a root
// under a symlink (macOS /var → /private/var) would put every file outside it.
func realDir(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}
