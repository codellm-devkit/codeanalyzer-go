package core_test

// Tests for --target-files / TargetFiles incremental analysis mode.
//
// When TargetFiles is non-empty each value is passed as a "file=<abs_path>"
// pattern to packages.Load, which loads only the package(s) containing those
// files.  Other packages in the project are not loaded and must not appear in
// the symbol table.

import (
	"path/filepath"
	"testing"

	"github.com/codellm-devkit/codeanalyzer-go/internal/core"
	"github.com/codellm-devkit/codeanalyzer-go/internal/options"
)

func multipackageDir() string {
	return filepath.Join(testdataDir(), "multipackage")
}

// TestTargetFiles_SinglePackage: targeting one file restricts the symbol table
// to the package containing that file.  The multipackage fixture has three
// packages (main, server, worker); targeting server/server.go should exclude
// main.go and worker/worker.go.
func TestTargetFiles_SinglePackage(t *testing.T) {
	td := multipackageDir()
	serverFile := filepath.Join(td, "server", "server.go")

	app, err := core.New(options.AnalysisOptions{
		InputPath:   td,
		OutputDir:   t.TempDir(),
		Level:       options.LevelSymbolTable,
		SkipTests:   true,
		CacheDir:    t.TempDir(),
		TargetFiles: []string{serverFile},
	}).Analyze()
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}

	if len(app.SymbolTable) == 0 {
		t.Fatal("symbol table is empty — analysis may have failed silently")
	}
	if _, ok := app.SymbolTable["server/server.go"]; !ok {
		t.Errorf("server/server.go should be in symbol table; got keys: %v", keys(app.SymbolTable))
	}
	if _, ok := app.SymbolTable["main.go"]; ok {
		t.Error("main.go must not be in symbol table when only server package is targeted")
	}
	if _, ok := app.SymbolTable["worker/worker.go"]; ok {
		t.Error("worker/worker.go must not be in symbol table when only server package is targeted")
	}
}

// TestTargetFiles_MultiplePackages: targeting files in two separate packages
// includes both packages but still excludes the third.
func TestTargetFiles_MultiplePackages(t *testing.T) {
	td := multipackageDir()

	app, err := core.New(options.AnalysisOptions{
		InputPath: td,
		OutputDir: t.TempDir(),
		Level:     options.LevelSymbolTable,
		SkipTests: true,
		CacheDir:  t.TempDir(),
		TargetFiles: []string{
			filepath.Join(td, "server", "server.go"),
			filepath.Join(td, "worker", "worker.go"),
		},
	}).Analyze()
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}

	for _, want := range []string{"server/server.go", "worker/worker.go"} {
		if _, ok := app.SymbolTable[want]; !ok {
			t.Errorf("%s should be in symbol table; got keys: %v", want, keys(app.SymbolTable))
		}
	}
	if _, ok := app.SymbolTable["main.go"]; ok {
		t.Error("main.go must not be in symbol table when not targeted")
	}
}

// TestTargetFiles_NilMeansAllFiles: nil TargetFiles produces a full analysis,
// matching the file count of the pre-computed sharedMultipackageL1.
func TestTargetFiles_NilMeansAllFiles(t *testing.T) {
	const want = 4 // main.go + server/server.go + server/middleware.go + worker/worker.go
	if got := len(sharedMultipackageL1.SymbolTable); got != want {
		t.Errorf("multipackage fixture with nil TargetFiles: got %d files, want %d; keys: %v",
			got, want, keys(sharedMultipackageL1.SymbolTable))
	}
}

// TestTargetFiles_SiblingFilesIncluded: when a package has multiple source files
// (server.go + middleware.go), targeting any one file loads the entire package,
// so sibling files are also present in the symbol table.
func TestTargetFiles_SiblingFilesIncluded(t *testing.T) {
	td := multipackageDir()
	serverFile := filepath.Join(td, "server", "server.go")

	app, err := core.New(options.AnalysisOptions{
		InputPath:   td,
		OutputDir:   t.TempDir(),
		Level:       options.LevelSymbolTable,
		SkipTests:   true,
		CacheDir:    t.TempDir(),
		TargetFiles: []string{serverFile},
	}).Analyze()
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}

	// middleware.go is in the same package as server.go; it must appear too.
	if _, ok := app.SymbolTable["server/middleware.go"]; !ok {
		t.Errorf("server/middleware.go (sibling file) should be in symbol table; got keys: %v",
			keys(app.SymbolTable))
	}
}
