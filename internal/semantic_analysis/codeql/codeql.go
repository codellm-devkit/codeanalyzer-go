package codeql

import (
	"github.com/codellm-devkit/codeanalyzer-go/internal/schema"
	"github.com/codellm-devkit/codeanalyzer-go/internal/utils"
)

// CodeQL is the top-level handle for CodeQL-backed analysis. core.go talks only
// to this type; it never touches the binary, database, or query strings directly.
// TODO(level-2): implement Build() and Edges().
type CodeQL struct {
	loader  *Loader
	runner  *Runner
	enabled bool
}

// New probes for the CodeQL binary and returns a CodeQL handle.
// If the binary is absent and enabled=true, returns an error.
func New(cacheDir string, enabled bool) (*CodeQL, error) {
	if !enabled {
		return &CodeQL{enabled: false}, nil
	}
	loader, err := NewLoader()
	if err != nil {
		return nil, err
	}
	runner := NewRunner(loader, cacheDir+"/codeql-db")
	return &CodeQL{loader: loader, runner: runner, enabled: true}, nil
}

// Build creates the CodeQL database for projectDir.
// No-op when CodeQL is disabled.
func (c *CodeQL) Build(projectDir string) error {
	if !c.enabled {
		return nil
	}
	utils.Info("building CodeQL database (stub — TODO level-2)")
	return c.runner.BuildDatabase(projectDir)
}

// Edges returns Tier-2 call-graph edges.
// Returns empty slice when disabled; returns ErrCodeQLNotImplemented when enabled (stub).
func (c *CodeQL) Edges() ([]schema.GoCallEdge, error) {
	if !c.enabled {
		return nil, nil
	}
	return c.runner.QueryCallGraph()
}
