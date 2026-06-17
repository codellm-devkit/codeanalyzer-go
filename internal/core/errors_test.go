package core_test

// Error-path tests: verify that the analyzer returns meaningful errors (not
// panics or silent empty results) when given bad inputs.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/codellm-devkit/codeanalyzer-go/internal/core"
	"github.com/codellm-devkit/codeanalyzer-go/internal/options"
)

func TestAnalyze_NonExistentPath(t *testing.T) {
	opts := options.AnalysisOptions{
		InputPath: filepath.Join(t.TempDir(), "does_not_exist"),
		OutputDir: t.TempDir(),
		Level:     options.LevelSymbolTable,
		SkipTests: true,
	}
	_, err := core.New(opts).Analyze()
	if err == nil {
		t.Fatal("expected error for non-existent InputPath, got nil")
	}
}

func TestAnalyze_EmptyDirectory(t *testing.T) {
	// A real directory with no Go files: analyzer should succeed but produce an
	// empty symbol table (graceful degradation, not a hard error).
	emptyDir := t.TempDir()
	opts := options.AnalysisOptions{
		InputPath: emptyDir,
		OutputDir: t.TempDir(),
		Level:     options.LevelSymbolTable,
		SkipTests: true,
	}
	app, err := core.New(opts).Analyze()
	if err != nil {
		t.Logf("Analyze returned error (acceptable): %v", err)
		return
	}
	if len(app.SymbolTable) != 0 {
		t.Errorf("expected empty symbol table for empty directory; got %d entries", len(app.SymbolTable))
	}
}

func TestAnalyze_MissingGoMod(t *testing.T) {
	// A directory with a .go file but no go.mod — not a valid module.
	// The analyzer either returns an error or an empty symbol table; both are acceptable.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hello.go"), []byte("package main\nfunc main(){}\n"), 0o644); err != nil {
		t.Fatalf("writing hello.go: %v", err)
	}
	opts := options.AnalysisOptions{
		InputPath: dir,
		OutputDir: t.TempDir(),
		Level:     options.LevelSymbolTable,
		SkipTests: true,
	}
	app, err := core.New(opts).Analyze()
	if err != nil {
		t.Logf("Analyze returned error (acceptable): %v", err)
		return
	}
	if len(app.SymbolTable) != 0 {
		t.Errorf("expected empty symbol table for module with no go.mod; got %d entries", len(app.SymbolTable))
	}
}

func TestAnalyze_LevelOneDoesNotProduceCallGraph(t *testing.T) {
	// Level-1 analysis must never populate the call graph.
	if len(sharedGreeterL1.CallGraph) != 0 {
		t.Errorf("LevelSymbolTable produced %d call-graph edges; expected 0", len(sharedGreeterL1.CallGraph))
	}
}
