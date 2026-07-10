// Package frameworks provides the base for entrypoint-finder passes.
//
// Concrete finders (gin-router, net/http handler, etc.) embed BaseEntrypointFinder
// and override FindEntrypoints. They register themselves via analysis.RegisterPass.
//
// This mirrors codeanalyzer-python's frameworks/_base.py.
package frameworks

import (
	"github.com/codellm-devkit/codeanalyzer-go/internal/analysis"
	"github.com/codellm-devkit/codeanalyzer-go/internal/schema"
)

// BaseEntrypointFinder is the abstract base for framework-specific entrypoint finders.
// Concrete implementations embed this struct and override FindEntrypoints.
type BaseEntrypointFinder struct {
	name      string
	framework string
}

// NewBaseEntrypointFinder constructs a finder with the given name and framework label.
func NewBaseEntrypointFinder(name, framework string) BaseEntrypointFinder {
	return BaseEntrypointFinder{name: name, framework: framework}
}

// Name implements analysis.AnalysisPass.
func (b BaseEntrypointFinder) Name() string { return b.name }

// Provides implements analysis.AnalysisPass — finders provide the framework name as a capability.
func (b BaseEntrypointFinder) Provides() []string { return []string{b.framework + ":entrypoints"} }

// Requires implements analysis.AnalysisPass — no dependencies by default.
func (b BaseEntrypointFinder) Requires() []string { return nil }

// Run implements analysis.AnalysisPass — delegates to FindEntrypoints.
func (b BaseEntrypointFinder) Run(app *schema.GoApplication, ctx analysis.AnalysisContext) (analysis.AnalysisResult, error) {
	return analysis.AnalysisResult{}, nil
}
