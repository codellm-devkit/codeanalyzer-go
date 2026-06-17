package core_test

// Tests for the SkipTests option.
//
// testdata/multipackage/server/server_test.go is a minimal test file whose sole
// purpose is to give these tests something to look for.  It is never included
// in the shared fixtures (all use SkipTests: true).

import (
	"path/filepath"
	"testing"

	"github.com/codellm-devkit/codeanalyzer-go/internal/core"
	"github.com/codellm-devkit/codeanalyzer-go/internal/options"
)

const serverTestFile = "server/server_test.go"

// TestSkipTests_TrueExcludesTestFiles: the default (SkipTests=true) must not
// include any *_test.go file in the symbol table.
func TestSkipTests_TrueExcludesTestFiles(t *testing.T) {
	// sharedMultipackageL1 is built with SkipTests: true — re-use it.
	for key := range sharedMultipackageL1.SymbolTable {
		if len(key) >= 8 && key[len(key)-8:] == "_test.go" {
			t.Errorf("SkipTests=true: found test file in symbol table: %s", key)
		}
	}
}

// TestSkipTests_FalseIncludesTestFiles: with SkipTests=false the analyzer must
// include *_test.go files in the symbol table.
func TestSkipTests_FalseIncludesTestFiles(t *testing.T) {
	app, err := core.New(options.AnalysisOptions{
		InputPath: filepath.Join(testdataDir(), "multipackage"),
		OutputDir: t.TempDir(),
		Level:     options.LevelSymbolTable,
		SkipTests: false,
		CacheDir:  t.TempDir(),
	}).Analyze()
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}

	if _, ok := app.SymbolTable[serverTestFile]; !ok {
		t.Errorf("SkipTests=false: %s not in symbol table; got keys: %v",
			serverTestFile, keys(app.SymbolTable))
	}
}

// TestSkipTests_FalseIncreasesFileCount: the symbol table with SkipTests=false
// must have more files than the same analysis with SkipTests=true.
func TestSkipTests_FalseIncreasesFileCount(t *testing.T) {
	withSkip := len(sharedMultipackageL1.SymbolTable)

	app, err := core.New(options.AnalysisOptions{
		InputPath: filepath.Join(testdataDir(), "multipackage"),
		OutputDir: t.TempDir(),
		Level:     options.LevelSymbolTable,
		SkipTests: false,
		CacheDir:  t.TempDir(),
	}).Analyze()
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}

	if len(app.SymbolTable) <= withSkip {
		t.Errorf("SkipTests=false: expected more files than %d (SkipTests=true count); got %d",
			withSkip, len(app.SymbolTable))
	}
}
