// codeanalyzer-go is the CLI entry point for the Go language analyzer.
//
// It exposes the standard CLDK CLI surface (cli-contract.md) so the Python SDK
// facade can shell out to it uniformly alongside Java and Python backends.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/codellm-devkit/codeanalyzer-go/internal/core"
	"github.com/codellm-devkit/codeanalyzer-go/internal/options"
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
		targetFiles   []string
		skipTests     bool
		eager         bool
		cacheDir      string
		jobs          int
		useCodeQL     bool
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

			// --emit selects the output projection. neo4j/schema are validated
			// here and rejected with a non-zero exit until the Neo4j child lands
			// (never a silent fallback to JSON). schema needs no --input.
			switch options.EmitTarget(emit) {
			case options.EmitJSON:
				// valid — falls through to the JSON path below.
			case options.EmitNeo4j:
				return fmt.Errorf("--emit neo4j is not yet implemented; use --emit json")
			case options.EmitSchema:
				return fmt.Errorf("--emit schema is not yet implemented; use --emit json")
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
				InputPath:     inputPath,
				OutputDir:     outputDir,
				Format:        format,
				Emit:          options.EmitTarget(emit),
				AppName:       appName,
				Level:         options.AnalysisLevel(level),
				TargetFiles:   targetFiles,
				SkipTests:     skipTests,
				Eager:         eager,
				CacheDir:      cacheDir,
				Jobs:          jobs,
				UseCodeQL:     useCodeQL,
				Verbose:       verbosity > 0,
				Neo4jURI:      firstNonEmpty(neo4jURI, os.Getenv("NEO4J_URI")),
				Neo4jUser:     firstNonEmpty(neo4jUser, os.Getenv("NEO4J_USERNAME"), "neo4j"),
				Neo4jPassword: firstNonEmpty(neo4jPassword, os.Getenv("NEO4J_PASSWORD"), "neo4j"),
				Neo4jDatabase: firstNonEmpty(neo4jDatabase, os.Getenv("NEO4J_DATABASE")),
			}

			analyzer := core.New(opts)
			app, err := analyzer.Analyze()
			if err != nil {
				return err
			}

			// When no --output dir is given, write JSON to cobra's output
			// writer so tests can capture it via cmd.SetOut.
			if outputDir == "" {
				data, err := json.Marshal(app)
				if err != nil {
					return err
				}
				_, err = cmd.OutOrStdout().Write(data)
				return err
			}
			return core.WriteOutput(app, outputDir, format)
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
		"Analysis level: 1=symbol table only, 2=+resolver call graph")
	f.StringSliceVarP(&targetFiles, "target-files", "t", nil,
		"Restrict analysis to specific files (incremental mode)")
	f.BoolVar(&skipTests, "skip-tests", true, "Skip *_test.go files")
	f.BoolVar(&eager, "eager", false, "Force clean rebuild (ignore cache)")
	f.StringVarP(&cacheDir, "cache-dir", "c", "", "Cache directory (default: ~/.cldk/go-cache)")
	f.IntVarP(&jobs, "jobs", "j", 0, "Worker parallelism (default: CPU cores)")
	f.BoolVar(&useCodeQL, "codeql", false, "Enable CodeQL framework-based call graph (level 2, stub)")
	f.CountVarP(&verbosity, "verbose", "v", "Verbosity (repeat for more detail)")
	f.BoolVar(&showVersion, "version", false, "Print version and exit")

	// Neo4j projection targets (validated now, consumed by the Neo4j child).
	f.StringVar(&neo4jURI, "neo4j-uri", "", "Live Bolt push target (env NEO4J_URI); omit to write graph.cypher")
	f.StringVar(&neo4jUser, "neo4j-user", "", "Neo4j username (env NEO4J_USERNAME, default neo4j)")
	f.StringVar(&neo4jPassword, "neo4j-password", "", "Neo4j password (env NEO4J_PASSWORD, default neo4j)")
	f.StringVar(&neo4jDatabase, "neo4j-database", "", "Neo4j database (env NEO4J_DATABASE, optional)")

	return cmd
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
