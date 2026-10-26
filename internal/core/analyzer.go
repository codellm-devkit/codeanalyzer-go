// Package core is the ORCHESTRATOR for codeanalyzer-go analysis.
//
// Analyzer.Analyze() delegates each phase to its own package; it inlines no
// analysis logic and never hardcodes entrypoints. This mirrors the structural
// discipline of codeanalyzer-python/codeanalyzer/core.py.
//
// Phase order:
//  1. Project materialization (go mod download)
//  2. Symbol table construction  (syntactic_analysis)
//  3. Resolver-based call graph  (semantic_analysis) — if level >= 2
//  4. Pass pipeline              (analysis/registry)
//  5. Intraprocedural dataflow   (dataflow) — if level >= 3
//  6. System dependence graph    (sdg) — if level >= 4
package core

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/codellm-devkit/codeanalyzer-go/internal/analysis"
	"github.com/codellm-devkit/codeanalyzer-go/internal/dataflow"
	"github.com/codellm-devkit/codeanalyzer-go/internal/options"
	"github.com/codellm-devkit/codeanalyzer-go/internal/schema"
	"github.com/codellm-devkit/codeanalyzer-go/internal/sdg"
	"github.com/codellm-devkit/codeanalyzer-go/internal/semantic_analysis"
	"github.com/codellm-devkit/codeanalyzer-go/internal/syntactic_analysis"
	"github.com/codellm-devkit/codeanalyzer-go/internal/syntactic_analysis/v2emit"
	"github.com/codellm-devkit/codeanalyzer-go/internal/utils"
)

// Analyzer is the top-level analysis driver. Construct with New() and call Analyze().
type Analyzer struct {
	opts options.AnalysisOptions
	// l3 is the Phase 5 side table (nil below L3). It stays off
	// schema.GoApplication: schema must not depend on dataflow, and the
	// cached v1 model has no L3 facts.
	l3 map[dataflow.L3Key]dataflow.Result
	// l4 is the Phase 6 result (nil below L4). Pass it to RenderJSON /
	// WriteOutput / v2emit.Emit with L3().
	l4 *sdg.Result
}

// L3 returns the dataflow side table of the last Analyze run: one result per
// callable with a body, keyed by (declaring file, signature). Nil below
// level 3. Pass it to RenderJSON / WriteOutput / v2emit.Emit.
func (a *Analyzer) L3() map[dataflow.L3Key]dataflow.Result { return a.l3 }

// L4 returns the SDG result of the last Analyze run. Nil below level 4.
func (a *Analyzer) L4() *sdg.Result { return a.l4 }

// KLimit is the access-path bound used at L3: opts.FieldDepth, or the
// --graph-field-depth default of 3 when a non-CLI caller left it 0.
func KLimit(opts options.AnalysisOptions) int {
	if opts.FieldDepth > 0 {
		return opts.FieldDepth
	}
	return 3
}

// New creates an Analyzer for the given options.
func New(opts options.AnalysisOptions) *Analyzer {
	return &Analyzer{opts: opts}
}

// Analyze runs the full analysis pipeline and returns a GoApplication.
func (a *Analyzer) Analyze() (*schema.GoApplication, error) {
	// Resolve to absolute path so filepath.Rel works correctly in all cases.
	abs, err := filepath.Abs(a.opts.InputPath)
	if err != nil {
		return nil, fmt.Errorf("resolving input path: %w", err)
	}
	a.opts.InputPath = abs
	utils.Info("analyzing project: %s", a.opts.InputPath)

	// The L4 library models are read first, so a bad file fails the run
	// before any analysis (also under --emit neo4j -t, which runs at L3).
	models, err := sdg.LoadModels(a.opts.LibraryModels)
	if err != nil {
		return nil, err
	}

	// ── Phase 1: Project materialization ──────────────────────────────────────
	if err := a.materialize(); err != nil {
		// Degrade gracefully — log but don't abort. Partial types are better than nothing.
		utils.Warn("dependency materialization failed: %v (continuing with partial types)", err)
	}

	// ── Phase 2: Symbol table construction ───────────────────────────────────
	builder := syntactic_analysis.NewSymbolTableBuilder(a.opts.InputPath)
	symbolTable, err := builder.Build(a.opts.TargetFiles, a.opts.SkipTests)
	if err != nil {
		return nil, fmt.Errorf("symbol table construction failed: %w", err)
	}
	utils.Info("symbol table: %d files", len(symbolTable))

	app := &schema.GoApplication{
		SymbolTable: symbolTable,
		CallGraph:   []schema.GoCallEdge{},
		Entrypoints: map[string][]schema.GoEntrypoint{},
	}

	if a.opts.Level < options.LevelCallGraph {
		// Level 1 (symbol-table only) — skip call graph and passes.
		return a.finalizeAndCache(app)
	}

	// ── Phase 3: Resolver-based call graph ────────────────────────────────────
	cgBuilder := semantic_analysis.NewCallGraphBuilder(
		a.opts.InputPath, builder.Fset(), builder.Pkgs(),
	)
	edges := cgBuilder.Build(symbolTable)
	app.CallGraph = edges
	app.Externals = cgBuilder.Externals()
	utils.Info("call graph: %d edges", len(edges))

	// ── Phase 4: Pass pipeline ────────────────────────────────────────────────
	ctx := analysis.AnalysisContext{
		ProjectDir: a.opts.InputPath,
		CacheDir:   a.opts.CacheDir,
	}
	if err := analysis.RunPipeline(app, ctx); err != nil {
		utils.Warn("pass pipeline error: %v", err)
	}

	// ── Phase 5: Intraprocedural dataflow (L3) ────────────────────────────────
	var units []dataflow.Unit
	if a.opts.Level >= options.LevelDataflow {
		units = builder.L3Units(symbolTable)
		if a.opts.Level >= options.LevelSDG {
			// L4 reads each result's def/use sets and call keys (the SSA
			// join); Release drops them after sdg.Run.
			a.l3 = dataflow.RunWithSideTables(builder.Fset(), units, KLimit(a.opts), a.opts.Jobs)
		} else {
			a.l3 = dataflow.Run(builder.Fset(), units, KLimit(a.opts), a.opts.Jobs)
		}
		utils.Info("dataflow: %d callables", len(a.l3))
	}

	// ── Phase 6: System dependence graph (L4) ─────────────────────────────────
	if a.opts.Level >= options.LevelSDG {
		res := sdg.Run(sdg.Input{
			Fset:        builder.Fset(),
			Pkgs:        builder.Pkgs(),
			ProjectDir:  a.opts.InputPath,
			SymbolTable: symbolTable,
			Externals:   app.Externals,
			CallGraph:   app.CallGraph,
			Units:       units,
			L3:          a.l3,
			K:           KLimit(a.opts),
			Jobs:        a.opts.Jobs,
			Models:      models,
		})
		a.l4 = &res
		if err := a.reportDegraded(res.Degraded); err != nil {
			return nil, err
		}
		// VTA's dispatch edges and the externals only VTA reached (spec §4.8):
		// additions to the L2 call graph, so -a 3 ⊆ -a 4.
		res.ApplyTo(app)
	}

	// The last reader of Node.Stmt and of the L4 side tables is done
	// (dataflow.Run, or sdg.Run at L4, which joins SSA to them and binds
	// call-site actuals from the AST; the emitters read keys, spans and
	// edges only), so drop them now: the parsed packages are then
	// unreachable once Analyze returns, before the cache write and the
	// emission. (#27)
	dataflow.ReleaseAll(a.l3)

	return a.finalizeAndCache(app)
}

// reportDegraded logs one warning per L4 degradation, or, under --strict,
// fails with an error naming every degraded code.
func (a *Analyzer) reportDegraded(deg []sdg.Degraded) error {
	if len(deg) == 0 {
		return nil
	}
	if a.opts.Strict {
		codes := make([]string, len(deg))
		for i, d := range deg {
			codes[i] = d.Code
		}
		return fmt.Errorf("--strict: analysis level 4 degraded: %s", strings.Join(codes, ", "))
	}
	for _, d := range deg {
		msg := "L4 degraded: " + d.Code
		if d.Detail != "" {
			msg += " (" + d.Detail + ")"
		}
		if len(d.Packages) > 0 {
			msg += ": " + strings.Join(d.Packages, ", ")
		}
		utils.Warn("%s", msg)
	}
	return nil
}

// materialize runs `go mod download` to ensure the module graph is available
// for go/packages to resolve imports and types. Idempotent and cached.
func (a *Analyzer) materialize() error {
	goModPath := filepath.Join(a.opts.InputPath, "go.mod")
	if _, err := os.Stat(goModPath); os.IsNotExist(err) {
		utils.Debug("no go.mod found at %s; skipping go mod download", a.opts.InputPath)
		return nil
	}

	// Check cache: if the go.sum hasn't changed, skip download.
	if !a.opts.Eager {
		goSumPath := filepath.Join(a.opts.InputPath, "go.sum")
		cacheKey := filepath.Join(a.opts.CacheDir, "go_mod_hash")
		if currentHash, err := utils.FileHash(goSumPath); err == nil {
			if cachedHash, err := os.ReadFile(cacheKey); err == nil && string(cachedHash) == currentHash {
				utils.Debug("go mod download: cache hit, skipping")
				return nil
			}
		}
		defer func() {
			goSumPath := filepath.Join(a.opts.InputPath, "go.sum")
			if currentHash, err := utils.FileHash(goSumPath); err == nil {
				_ = utils.EnsureDir(a.opts.CacheDir)
				_ = os.WriteFile(cacheKey, []byte(currentHash), 0o644)
			}
		}()
	}

	utils.Info("running go mod download...")
	cmd := exec.Command("go", "mod", "download")
	cmd.Dir = a.opts.InputPath
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// finalizeAndCache caches the application and returns it.
func (a *Analyzer) finalizeAndCache(app *schema.GoApplication) (*schema.GoApplication, error) {
	if a.opts.CacheDir != "" {
		_ = a.saveCache(app)
	}
	return app, nil
}

// saveCache persists the application to cache as analysis_cache.json.
func (a *Analyzer) saveCache(app *schema.GoApplication) error {
	if err := utils.EnsureDir(a.opts.CacheDir); err != nil {
		return err
	}
	cachePath := filepath.Join(a.opts.CacheDir, "analysis_cache.json")
	data, err := json.Marshal(app)
	if err != nil {
		return err
	}
	return os.WriteFile(cachePath, data, 0o644)
}

// checkFormat rejects every output format but json (the default when empty)
// with an explicit error rather than silently falling back.
func checkFormat(opts options.AnalysisOptions) error {
	switch opts.Format {
	case "", "json":
		return nil
	case "msgpack":
		return fmt.Errorf("msgpack output is not yet implemented; use --format json")
	default:
		return fmt.Errorf("unsupported output format %q; supported: json", opts.Format)
	}
}

// RenderJSON serializes the analysis to JSON as the canonical v2 containment
// tree via the v2emit emitter, as one []byte. It is the reference the
// streaming StreamJSON / WriteOutput paths are byte-identical to; the CLI
// streams instead, so the document never exists in memory at once.
//
// l3 is the Analyzer's L3() side table (nil below level 3) and l4 its L4()
// result (nil below level 4). At level 3 the envelope carries k_limit =
// KLimit(opts) whether or not l3 is set.
func RenderJSON(app *schema.GoApplication, l3 map[dataflow.L3Key]dataflow.Result, l4 *sdg.Result, opts options.AnalysisOptions) ([]byte, error) {
	if err := checkFormat(opts); err != nil {
		return nil, err
	}
	payload := v2emit.Emit(app, opts.AppName, opts.InputPath, int(opts.Level), opts.AnalyzerVersion, KLimit(opts), l3, l4)
	return json.Marshal(payload)
}

// StreamJSON writes the bytes RenderJSON returns to w, emitting and encoding
// one module at a time (v2emit.EmitTo) through a buffered writer.
func StreamJSON(w io.Writer, app *schema.GoApplication, l3 map[dataflow.L3Key]dataflow.Result, l4 *sdg.Result, opts options.AnalysisOptions) error {
	if err := checkFormat(opts); err != nil {
		return err
	}
	bw := bufio.NewWriterSize(w, 1<<20)
	if err := v2emit.EmitTo(bw, app, opts.AppName, opts.InputPath, int(opts.Level), opts.AnalyzerVersion, KLimit(opts), l3, l4); err != nil {
		return err
	}
	return bw.Flush()
}

// WriteOutput streams the analysis to outputDir/analysis.json, or to stdout
// when outputDir is empty. The file is written beside its destination and
// renamed into place, so a failed run never leaves a truncated analysis.json.
func WriteOutput(app *schema.GoApplication, l3 map[dataflow.L3Key]dataflow.Result, l4 *sdg.Result, opts options.AnalysisOptions) error {
	if err := checkFormat(opts); err != nil {
		return err
	}
	if opts.OutputDir == "" {
		return StreamJSON(os.Stdout, app, l3, l4, opts)
	}
	if err := utils.EnsureDir(opts.OutputDir); err != nil {
		return err
	}
	dst := filepath.Join(opts.OutputDir, "analysis.json")
	f, err := os.CreateTemp(opts.OutputDir, ".analysis.json.*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	err = StreamJSON(f, app, l3, l4, opts)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp, 0o644)
	}
	if err == nil {
		err = os.Rename(tmp, dst)
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err
}
