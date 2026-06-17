// Package analysis — registry discovers, orders, and runs passes.
package analysis

import (
	"fmt"

	"github.com/codellm-devkit/codeanalyzer-go/internal/schema"
	"github.com/codellm-devkit/codeanalyzer-go/internal/utils"
)

var registeredPasses []AnalysisPass

// RegisterPass adds a pass to the built-in registry. Call from init() in each
// pass file to register without touching the registry directly.
func RegisterPass(p AnalysisPass) {
	registeredPasses = append(registeredPasses, p)
}

// orderPasses performs a topological sort by Requires/Provides.
// Returns an error if a dependency is unsatisfied or a cycle exists.
func orderPasses(passes []AnalysisPass) ([]AnalysisPass, error) {
	provided := map[string]bool{}
	var ordered []AnalysisPass
	remaining := make([]AnalysisPass, len(passes))
	copy(remaining, passes)

	for len(remaining) > 0 {
		progress := false
		var next []AnalysisPass
		for _, p := range remaining {
			ready := true
			for _, req := range p.Requires() {
				if !provided[req] {
					ready = false
					break
				}
			}
			if ready {
				ordered = append(ordered, p)
				for _, cap := range p.Provides() {
					provided[cap] = true
				}
				progress = true
			} else {
				next = append(next, p)
			}
		}
		if !progress {
			return nil, fmt.Errorf("unsatisfied pass dependencies or cycle among: %v",
				func() []string {
					names := make([]string, len(remaining))
					for i, p := range remaining {
						names[i] = p.Name()
					}
					return names
				}())
		}
		remaining = next
	}
	return ordered, nil
}

// RunPipeline runs all registered passes over app in dependency order,
// merging each result into the running application before the next pass.
// Pass output is deliberately not cached — out-of-tree enrichment must not go stale.
func RunPipeline(app *schema.GoApplication, ctx AnalysisContext) error {
	ordered, err := orderPasses(registeredPasses)
	if err != nil {
		return err
	}
	if len(ordered) == 0 {
		utils.Debug("no registered analysis passes; skipping pipeline")
		return nil
	}
	for _, p := range ordered {
		utils.Info("running pass: %s", p.Name())
		result, err := p.Run(app, ctx)
		if err != nil {
			utils.Warn("pass %s failed: %v (continuing)", p.Name(), err)
			continue
		}
		// Merge entrypoints
		for framework, eps := range result.Entrypoints {
			app.Entrypoints[framework] = append(app.Entrypoints[framework], eps...)
		}
		// Merge synthetic edges
		app.CallGraph = append(app.CallGraph, result.SyntheticEdges...)
	}
	return nil
}
