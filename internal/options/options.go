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

// EmitTarget selects the analyzer's output projection (cli-contract.md § Neo4j).
type EmitTarget string

const (
	// EmitJSON writes the analysis.json projection (the default).
	EmitJSON EmitTarget = "json"
	// EmitNeo4j writes/pushes the Neo4j graph projection at full implemented depth.
	EmitNeo4j EmitTarget = "neo4j"
	// EmitSchema writes the static schema.neo4j.json contract (needs no --input).
	EmitSchema EmitTarget = "schema"
)

// AnalysisOptions is the configuration surface passed from the CLI into Analyzer.
type AnalysisOptions struct {
	// InputPath is the project root to analyze.
	InputPath string
	// OutputDir is where analysis.json is written. Empty = write to stdout.
	OutputDir string
	// Format is the serialization format: "json" or "msgpack".
	Format string
	// Emit selects the output projection: json (default), neo4j, or schema.
	Emit EmitTarget
	// AppName anchors application identity (can://go/<app-name> and the Neo4j
	// :Application node). Defaults to the input directory's base name.
	AppName string
	// AnalysisLevel controls symbol-table-only (1) vs + call graph (2).
	Level AnalysisLevel
	// AnalyzerVersion is the analyzer's own version, recorded in the v2
	// manifest's analyzer{} tag. Set by the CLI from its build-stamped value.
	AnalyzerVersion string
	// TargetFiles restricts analysis to specific files (incremental mode).
	TargetFiles []string
	// SkipTests skips test files (files ending in _test.go).
	SkipTests bool
	// Eager forces a clean rebuild ignoring any cache.
	Eager bool
	// CacheDir is where per-file caches and intermediate data are stored.
	CacheDir string
	// Jobs is the worker parallelism (default: CPU cores). Output must be
	// byte-identical across Jobs values.
	Jobs int
	// Verbose enables verbose logging.
	Verbose bool

	// Neo4j projection targets (consumed by the Neo4j emitter; validated now).
	// Precedence for each: explicit flag > env var > default.
	Neo4jURI      string // live Bolt push target; empty → write graph.cypher. Env NEO4J_URI
	Neo4jUser     string // env NEO4J_USERNAME, default "neo4j"
	Neo4jPassword string // env NEO4J_PASSWORD, default "neo4j"
	Neo4jDatabase string // env NEO4J_DATABASE, optional
}
