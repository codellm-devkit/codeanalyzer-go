// Package options defines the AnalysisOptions passed from the CLI into core.
package options

// AnalysisLevel controls how much analysis is performed.
type AnalysisLevel int

const (
	// LevelSymbolTable produces the symbol table only (no call graph).
	LevelSymbolTable AnalysisLevel = 1
	// LevelCallGraph produces symbol table + resolver-based call graph (still cheap).
	LevelCallGraph AnalysisLevel = 2
)

// AnalysisOptions is the configuration surface passed from the CLI into Analyzer.
type AnalysisOptions struct {
	// InputPath is the project root to analyze.
	InputPath string
	// OutputDir is where analysis.json is written. Empty = write to stdout.
	OutputDir string
	// Format is the serialization format: "json" or "msgpack".
	Format string
	// AnalysisLevel controls symbol-table-only (1) vs + call graph (2).
	Level AnalysisLevel
	// TargetFiles restricts analysis to specific files (incremental mode).
	TargetFiles []string
	// SkipTests skips test files (files ending in _test.go).
	SkipTests bool
	// Eager forces a clean rebuild ignoring any cache.
	Eager bool
	// CacheDir is where per-file caches and intermediate data are stored.
	CacheDir string
	// UseCodeQL enables the framework-based (Tier-2) CodeQL call graph.
	UseCodeQL bool
	// Verbose enables verbose logging.
	Verbose bool
}
