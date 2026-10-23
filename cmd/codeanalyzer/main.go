// codeanalyzer-go is the CLI entry point for the Go language analyzer.
//
// It exposes the standard CLDK CLI surface (cli-contract.md) so the Python SDK
// facade can shell out to it uniformly alongside Java and Python backends.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/codellm-devkit/codeanalyzer-go/internal/core"
	"github.com/codellm-devkit/codeanalyzer-go/internal/neo4j"
	"github.com/codellm-devkit/codeanalyzer-go/internal/options"
	"github.com/codellm-devkit/codeanalyzer-go/internal/syntactic_analysis/v2emit"
	"github.com/codellm-devkit/codeanalyzer-go/internal/utils"
)

// version is the analyzer version. It defaults to a dev value and is overridden
// at release time via -ldflags "-X main.version=<tag>" (see packaging/python/build_wheels.sh),
// keeping the binary, the PyPI wheel, and the git tag in lockstep.
var version = "0.1.0"

func main() {
	if err := rootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func rootCmd() *cobra.Command {
	var (
		inputPath     string
		outputDir     string
		format        string
		emit          string
		appName       string
		level         int
		fieldDepth    int
		targetFiles   []string
		skipTests     bool
		eager         bool
		cacheDir      string
		jobs          int
		verbosity     int
		showVersion   bool
		neo4jURI      string
		neo4jUser     string
		neo4jPassword string
		neo4jDatabase string
	)

	cmd := &cobra.Command{
		Use:     "cango",
		Aliases: []string{"codeanalyzer-go"},
		Short:   "Static analysis for Go — symbol table and call graph via go/types",
		Long: `codeanalyzer-go produces analysis.json (symbol table + call graph) for Go projects.

The output conforms to the CLDK canonical schema so the Python SDK can load it
via CLDK(language="go").analysis(project_path=...).`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if showVersion {
				cmd.Println("cango " + version)
				return nil
			}

			// --emit selects the output projection. schema is a static contract
			// that needs no --input: handle it here and return. neo4j shares the
			// analysis path below but is ALWAYS full-depth, so an explicit
			// -a/--analysis-level alongside it is a flag error (the level gate
			// applies to the JSON path only).
			emitTarget := options.EmitTarget(emit)
			switch emitTarget {
			case options.EmitJSON:
				if err := validateAnalysisLevel(level); err != nil {
					return err
				}
				if err := validateFieldDepth(cmd, level, fieldDepth); err != nil {
					return err
				}
			case options.EmitNeo4j:
				if cmd.Flags().Changed("analysis-level") {
					return fmt.Errorf("--analysis-level does not apply to --emit neo4j; the graph is always projected at full depth")
				}
				if cmd.Flags().Changed("graph-field-depth") {
					return fmt.Errorf("--graph-field-depth does not apply to --emit neo4j; the graph is always projected at full depth")
				}
				// Force full depth: the graph carries every implemented
				// level's facts in both projections. (Output is always the
				// canonical v2 tree.)
				level = int(options.LevelDataflow)
			case options.EmitSchema:
				path, err := neo4j.EmitSchema(outputDir)
				if err != nil {
					return err
				}
				cmd.Println("wrote " + path)
				return nil
			default:
				return fmt.Errorf("unsupported --emit target %q; supported: json, neo4j, schema", emit)
			}

			if inputPath == "" {
				return fmt.Errorf("--input / -i is required")
			}
			switch format {
			case "", "json":
				// valid
			case "msgpack":
				return fmt.Errorf("msgpack output is not yet implemented; use --format json")
			default:
				return fmt.Errorf("unsupported output format %q; supported: json", format)
			}
			utils.SetVerbosity(verbosity)

			if cacheDir == "" {
				home, _ := os.UserHomeDir()
				cacheDir = home + "/.cldk/go-cache"
			}
			if jobs < 1 {
				jobs = runtime.NumCPU()
			}
			// application anchor: explicit flag > input dir base name.
			if appName == "" {
				appName = filepath.Base(filepath.Clean(inputPath))
			}

			opts := options.AnalysisOptions{
				InputPath:       inputPath,
				OutputDir:       outputDir,
				Format:          format,
				Emit:            options.EmitTarget(emit),
				AppName:         appName,
				Level:           options.AnalysisLevel(level),
				AnalyzerVersion: version,
				TargetFiles:     targetFiles,
				SkipTests:       skipTests,
				Eager:           eager,
				CacheDir:        cacheDir,
				Jobs:            jobs,
				Verbose:         verbosity > 0,
				FieldDepth:      fieldDepth,
				Neo4jURI:        firstNonEmpty(neo4jURI, os.Getenv("NEO4J_URI")),
				Neo4jUser:       firstNonEmpty(neo4jUser, os.Getenv("NEO4J_USERNAME"), "neo4j"),
				Neo4jPassword:   firstNonEmpty(neo4jPassword, os.Getenv("NEO4J_PASSWORD"), "neo4j"),
				Neo4jDatabase:   firstNonEmpty(neo4jDatabase, os.Getenv("NEO4J_DATABASE")),
			}

			analyzer := core.New(opts)
			app, err := analyzer.Analyze()
			if err != nil {
				return err
			}

			// Neo4j projection: build the canonical v2 payload (the SAME tree the
			// JSON path emits) and project it. --neo4j-uri set → live Bolt push;
			// absent → a graph.cypher snapshot in the output dir.
			if emitTarget == options.EmitNeo4j {
				payload := v2emit.Emit(app, opts.AppName, opts.InputPath, int(opts.Level), opts.AnalyzerVersion)
				if opts.Level >= options.LevelDataflow {
					payload.KLimit = opts.FieldDepth
				}
				boltCfg := neo4j.BoltConfig{
					URI:      opts.Neo4jURI,
					User:     opts.Neo4jUser,
					Password: opts.Neo4jPassword,
					Database: opts.Neo4jDatabase,
					Eager:    opts.Eager,
					FullRun:  len(opts.TargetFiles) == 0,
				}
				msg, err := neo4j.EmitNeo4j(cmd.Context(), payload, outputDir, boltCfg)
				if err != nil {
					return err
				}
				cmd.Println(msg)
				return nil
			}

			// When no --output dir is given, write JSON to cobra's output
			// writer so tests can capture it via cmd.SetOut. Both paths render
			// through core so the v1/v2 schema selection stays in one place.
			if outputDir == "" {
				data, err := core.RenderJSON(app, opts)
				if err != nil {
					return err
				}
				_, err = cmd.OutOrStdout().Write(data)
				return err
			}
			return core.WriteOutput(app, opts)
		},
	}

	f := cmd.Flags()
	f.StringVarP(&inputPath, "input", "i", "", "Project root to analyze (required)")
	f.StringVarP(&outputDir, "output", "o", "", "Output directory for analysis.json (default: stdout)")
	f.StringVarP(&format, "format", "f", "json", "Output format: json|msgpack")
	f.StringVar(&emit, "emit", "json", "Output projection: json|neo4j|schema")
	f.StringVar(&appName, "app-name", "",
		"Application anchor name for can:// ids and Neo4j :Application (default: input dir name)")
	f.IntVarP(&level, "analysis-level", "a", 1,
		"Analysis level: 1=symbol table, 2=+call graph, 3=+intraprocedural cfg/cdg/ddg")
	f.IntVar(&fieldDepth, "graph-field-depth", 3,
		"Field-path depth bound for L3 dataflow nodes (k limit; applies at -a 3 only)")
	f.StringSliceVarP(&targetFiles, "target-files", "t", nil,
		"Restrict analysis to specific files (incremental mode)")
	f.BoolVar(&skipTests, "skip-tests", true, "Skip *_test.go files")
	f.BoolVar(&eager, "eager", false, "Force clean rebuild (ignore cache)")
	f.StringVarP(&cacheDir, "cache-dir", "c", "", "Cache directory (default: ~/.cldk/go-cache)")
	f.IntVarP(&jobs, "jobs", "j", 0, "Worker parallelism (default: CPU cores)")
	f.CountVarP(&verbosity, "verbose", "v", "Verbosity (repeat for more detail)")
	f.BoolVar(&showVersion, "version", false, "Print version and exit")

	// Neo4j projection targets (validated now, consumed by the Neo4j child).
	f.StringVar(&neo4jURI, "neo4j-uri", "", "Live Bolt push target (env NEO4J_URI); omit to write graph.cypher")
	f.StringVar(&neo4jUser, "neo4j-user", "", "Neo4j username (env NEO4J_USERNAME, default neo4j)")
	f.StringVar(&neo4jPassword, "neo4j-password", "", "Neo4j password (env NEO4J_PASSWORD, default neo4j)")
	f.StringVar(&neo4jDatabase, "neo4j-database", "", "Neo4j database (env NEO4J_DATABASE, optional)")

	return cmd
}

// validateAnalysisLevel accepts the implemented levels (1..3). Level 4 is
// reserved for a later release and gets its own message.
func validateAnalysisLevel(level int) error {
	switch {
	case level >= 1 && level <= int(options.LevelDataflow):
		return nil
	case level == 4:
		return fmt.Errorf("analysis level 4 is not implemented yet")
	default:
		return fmt.Errorf("analysis level %d is not supported; use 1, 2 or 3", level)
	}
}

// validateFieldDepth checks --graph-field-depth. The flag only means something
// at L3+, and the k limit must be at least 1.
func validateFieldDepth(cmd *cobra.Command, level, fieldDepth int) error {
	if cmd.Flags().Changed("graph-field-depth") && level < int(options.LevelDataflow) {
		return fmt.Errorf("--graph-field-depth applies only at --analysis-level 3 or higher")
	}
	if fieldDepth < 1 {
		return fmt.Errorf("--graph-field-depth must be >= 1; got %d", fieldDepth)
	}
	return nil
}

// firstNonEmpty returns the first non-empty string in vals, or "" if all are
// empty. It encodes the contract's precedence: explicit flag > env var > default.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
