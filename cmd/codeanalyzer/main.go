// codeanalyzer-go is the CLI entry point for the Go language analyzer.
//
// It exposes the standard CLDK CLI surface (cli-contract.md) so the Python SDK
// facade can shell out to it uniformly alongside Java and Python backends.
package main

import (
	"encoding/json"
	"fmt"
	"os"

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
		inputPath   string
		outputDir   string
		format      string
		level       int
		targetFiles []string
		skipTests   bool
		eager       bool
		cacheDir    string
		useCodeQL   bool
		verbosity   int
		showVersion bool
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

			opts := options.AnalysisOptions{
				InputPath:   inputPath,
				OutputDir:   outputDir,
				Format:      format,
				Level:       options.AnalysisLevel(level),
				TargetFiles: targetFiles,
				SkipTests:   skipTests,
				Eager:       eager,
				CacheDir:    cacheDir,
				UseCodeQL:   useCodeQL,
				Verbose:     verbosity > 0,
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
	f.IntVarP(&level, "analysis-level", "a", 1,
		"Analysis level: 1=symbol table only, 2=+resolver call graph")
	f.StringSliceVarP(&targetFiles, "target-files", "t", nil,
		"Restrict analysis to specific files (incremental mode)")
	f.BoolVar(&skipTests, "skip-tests", true, "Skip *_test.go files")
	f.BoolVar(&eager, "eager", false, "Force clean rebuild (ignore cache)")
	f.StringVarP(&cacheDir, "cache-dir", "c", "", "Cache directory (default: ~/.cldk/go-cache)")
	f.BoolVar(&useCodeQL, "codeql", false, "Enable CodeQL framework-based call graph (level 2, stub)")
	f.CountVarP(&verbosity, "verbose", "v", "Verbosity (repeat for more detail)")
	f.BoolVar(&showVersion, "version", false, "Print version and exit")

	return cmd
}
