// Package analysis defines the pluggable pass layer for codeanalyzer-go.
//
// An AnalysisPass enriches a GoApplication after the base analysis (symbol table
// + call graph) is built. Passes declare capability tokens in Provides/Requires
// and are ordered topologically by the registry before running.
//
// This mirrors codeanalyzer-python's analysis/_pass.py. The seam exists so that
// codeanalyzer-extension-builder can register out-of-tree passes.
package analysis

import "github.com/codellm-devkit/codeanalyzer-go/internal/schema"

// AnalysisContext carries the shared context available to every pass.
type AnalysisContext struct {
	ProjectDir string
	CacheDir   string
}

// AnalysisResult holds the output of a single pass run.
type AnalysisResult struct {
	// Entrypoints discovered by this pass, keyed by framework name.
	Entrypoints map[string][]schema.GoEntrypoint
	// SyntheticEdges are additional call-graph edges contributed by this pass.
	SyntheticEdges []schema.GoCallEdge
}

// AnalysisPass is the interface every built-in and out-of-tree pass implements.
type AnalysisPass interface {
	// Name is the unique identifier for this pass (e.g. "gin-entrypoints").
	Name() string
	// Provides is the set of capability tokens this pass adds to the application.
	Provides() []string
	// Requires is the set of capability tokens that must have been provided before
	// this pass runs. The registry hard-errors on unsatisfied dependencies.
	Requires() []string
	// Run performs the analysis and returns its contributions.
	Run(app *schema.GoApplication, ctx AnalysisContext) (AnalysisResult, error)
}
