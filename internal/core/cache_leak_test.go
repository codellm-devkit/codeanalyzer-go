package core_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codellm-devkit/codeanalyzer-go/internal/core"
	"github.com/codellm-devkit/codeanalyzer-go/internal/options"
)

// The cache directory lives inside the project (-c ./cache). Each application
// writes its own identity-v2-<hash>/analysis_cache.json beneath it. Before the
// fix, the repository layer excluded only the current application's subdirectory,
// so run B recorded run A's cache as an artifact, with its full source under
// --artifact-text. Those bytes are another application's analysis.
//
// Practical shape of the project:
//
//	project/
//	  go.mod  main.go
//	  cache/
//	    identity-v2-<hash of appA>/analysis_cache.json   <- written by run A
//	    identity-v2-<hash of appB>/analysis_cache.json   <- written by run B
func TestAnalyze_CacheRootInsideProjectIsNotAnArtifact(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"go.mod":  "module example.com/cl\n\ngo 1.22\n",
		"main.go": "package main\n\nfunc main() {}\n",
	}
	for rel, content := range files {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cache := filepath.Join(dir, "cache")

	// Run A, then run B: B is the run whose output must not carry A's cache.
	for _, app := range []string{"appA", "appB"} {
		opts := options.AnalysisOptions{
			InputPath: dir, AppName: app, Level: options.LevelSymbolTable,
			SkipTests: true, CacheDir: cache, OutputDir: t.TempDir(), ArtifactText: true,
		}
		got, err := core.New(opts).Analyze()
		if err != nil {
			t.Fatalf("%s: %v", app, err)
		}
		for path := range got.Repository.Artifacts {
			if strings.HasPrefix(path, "cache/") || strings.Contains(path, "analysis_cache.json") {
				t.Errorf("%s recorded cache file %q as a project artifact", app, path)
			}
		}
		if _, ok := got.Repository.Artifacts["go.mod"]; !ok {
			t.Errorf("%s: go.mod missing from artifacts; the project itself must still be discovered", app)
		}
	}
}
