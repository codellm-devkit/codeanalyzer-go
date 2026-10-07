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
package core

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/codellm-devkit/codeanalyzer-go/internal/analysis"
	"github.com/codellm-devkit/codeanalyzer-go/internal/options"
	"github.com/codellm-devkit/codeanalyzer-go/internal/schema"
	"github.com/codellm-devkit/codeanalyzer-go/internal/semantic_analysis"
	"github.com/codellm-devkit/codeanalyzer-go/internal/syntactic_analysis"
	"github.com/codellm-devkit/codeanalyzer-go/internal/syntactic_analysis/v2emit"
	"github.com/codellm-devkit/codeanalyzer-go/internal/utils"
)

// Analyzer is the top-level analysis driver. Construct with New() and call Analyze().
type Analyzer struct {
	opts options.AnalysisOptions
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
	utils.Info("call graph: %d edges", len(edges))

	// ── Phase 4: Pass pipeline ────────────────────────────────────────────────
	ctx := analysis.AnalysisContext{
		ProjectDir: a.opts.InputPath,
		CacheDir:   a.opts.CacheDir,
	}
	if err := analysis.RunPipeline(app, ctx); err != nil {
		utils.Warn("pass pipeline error: %v", err)
	}

	return a.finalizeAndCache(app)
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

// RenderJSON serializes the analysis to JSON as the canonical v2 containment
// tree via the v2emit emitter. This is the single serialization point, so both
// the stdout and file output paths agree. Only "json" is supported; other
// formats return an explicit error rather than silently falling back.
func RenderJSON(app *schema.GoApplication, opts options.AnalysisOptions) ([]byte, error) {
	format := opts.Format
	if format == "" {
		format = "json"
	}
	switch format {
	case "json":
		// only supported format
	case "msgpack":
		return nil, fmt.Errorf("msgpack output is not yet implemented; use --format json")
	default:
		return nil, fmt.Errorf("unsupported output format %q; supported: json", format)
	}

	payload := v2emit.Emit(app, opts.AppName, opts.InputPath, int(opts.Level), opts.AnalyzerVersion)
	return json.Marshal(payload)
}

// WriteOutput serializes the analysis (in the selected schema) and writes it to
// outputDir/analysis.json, or to stdout when outputDir is empty.
func WriteOutput(app *schema.GoApplication, opts options.AnalysisOptions) error {
	data, err := RenderJSON(app, opts)
	if err != nil {
		return err
	}
	if opts.OutputDir == "" {
		_, err = os.Stdout.Write(data)
		return err
	}
	if err := utils.EnsureDir(opts.OutputDir); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(opts.OutputDir, "analysis.json"), data, 0o644)
}
