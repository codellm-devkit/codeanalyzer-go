package core_test

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/codellm-devkit/codeanalyzer-go/internal/core"
	"github.com/codellm-devkit/codeanalyzer-go/internal/options"
	"github.com/codellm-devkit/codeanalyzer-go/internal/schema"
)

// Shared analysis results, populated once in TestMain and reused across all tests.
// Caching tests are excluded: they must exercise the caching machinery themselves.
var (
	sharedGreeterL1      *schema.GoApplication // greeter, symbol-table only
	sharedGreeterL2      *schema.GoApplication // greeter, full call-graph
	sharedMultipackageL1 *schema.GoApplication // multipackage, symbol-table only
	sharedMultipackageL2 *schema.GoApplication // multipackage, full call-graph
	sharedGenericsL1  *schema.GoApplication // generics, symbol-table only
	sharedChiL2       *schema.GoApplication // chi (external dep), full call-graph
)

func TestMain(m *testing.M) {
	os.Exit(runTestMain(m))
}

// runTestMain wraps m.Run so that deferred cleanup runs before os.Exit.
func runTestMain(m *testing.M) int {
	tdRoot := testdataDir()

	tmpRoot, err := os.MkdirTemp("", "codeanalyzer-test-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "testsetup: MkdirTemp: %v\n", err)
		return 1
	}
	defer os.RemoveAll(tmpRoot)

	type fixture struct {
		name  string
		path  string
		level options.AnalysisLevel
		dst   **schema.GoApplication
	}

	for _, f := range []fixture{
		{"greeter/L1", filepath.Join(tdRoot, "greeter"), options.LevelSymbolTable, &sharedGreeterL1},
		{"greeter/L2", filepath.Join(tdRoot, "greeter"), options.LevelCallGraph, &sharedGreeterL2},
		{"multipackage/L1", filepath.Join(tdRoot, "multipackage"), options.LevelSymbolTable, &sharedMultipackageL1},
		{"multipackage/L2", filepath.Join(tdRoot, "multipackage"), options.LevelCallGraph, &sharedMultipackageL2},
		{"generics/L1", filepath.Join(tdRoot, "generics"), options.LevelSymbolTable, &sharedGenericsL1},
		{"chi/L2", filepath.Join(tdRoot, "chi"), options.LevelCallGraph, &sharedChiL2},
	} {
		outDir, err := os.MkdirTemp(tmpRoot, "out-*")
		if err != nil {
			fmt.Fprintf(os.Stderr, "testsetup %s: MkdirTemp out: %v\n", f.name, err)
			return 1
		}
		cacheDir, err := os.MkdirTemp(tmpRoot, "cache-*")
		if err != nil {
			fmt.Fprintf(os.Stderr, "testsetup %s: MkdirTemp cache: %v\n", f.name, err)
			return 1
		}
		opts := options.AnalysisOptions{
			InputPath: f.path,
			OutputDir: outDir,
			Level:     f.level,
			SkipTests: true,
			CacheDir:  cacheDir,
		}
		app, err := core.New(opts).Analyze()
		if err != nil {
			fmt.Fprintf(os.Stderr, "testsetup %s: Analyze: %v\n", f.name, err)
			return 1
		}
		if err := core.WriteOutput(app, outDir, "json"); err != nil {
			fmt.Fprintf(os.Stderr, "testsetup %s: WriteOutput: %v\n", f.name, err)
			return 1
		}
		if _, err := os.Stat(filepath.Join(outDir, "analysis.json")); err != nil {
			fmt.Fprintf(os.Stderr, "testsetup %s: analysis.json not created: %v\n", f.name, err)
			return 1
		}
		*f.dst = app
	}

	return m.Run()
}

// testdataDir returns the absolute path to the testdata directory.
// Uses runtime.Caller so it resolves correctly regardless of working directory.
func testdataDir() string {
	_, thisFile, _, _ := runtime.Caller(0)
	abs, _ := filepath.Abs(filepath.Join(filepath.Dir(thisFile), "..", "..", "testdata"))
	return abs
}

