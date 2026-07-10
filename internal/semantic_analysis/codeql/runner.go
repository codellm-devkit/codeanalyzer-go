package codeql

import "github.com/codellm-devkit/codeanalyzer-go/internal/schema"

// Runner builds a CodeQL database and runs queries to produce call-graph edges.
// TODO(level-2): implement database creation, query execution, and result parsing.
type Runner struct {
	loader *Loader
	dbDir  string
}

// NewRunner creates a Runner for the given project and database directory.
func NewRunner(loader *Loader, dbDir string) *Runner {
	return &Runner{loader: loader, dbDir: dbDir}
}

// BuildDatabase creates a CodeQL database for the Go project at projectDir.
// TODO(level-2): run `codeql database create --language=go`.
func (r *Runner) BuildDatabase(projectDir string) error {
	return ErrCodeQLNotImplemented
}

// QueryCallGraph runs the CodeQL call-graph query and returns edges.
// TODO(level-2): run query, parse SARIF/CSV, produce GoCallEdge list.
func (r *Runner) QueryCallGraph() ([]schema.GoCallEdge, error) {
	return nil, ErrCodeQLNotImplemented
}
