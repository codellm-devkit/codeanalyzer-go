package core_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/codellm-devkit/codeanalyzer-go/internal/core"
	"github.com/codellm-devkit/codeanalyzer-go/internal/options"
	v2 "github.com/codellm-devkit/codeanalyzer-go/internal/schema/v2"
	"github.com/codellm-devkit/codeanalyzer-go/internal/syntactic_analysis/v2emit"
)

// repoProject is a Go module with no requires (so no network materialization),
// one .go file, and two non-.go files.
func repoProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"go.mod":    "module example.com/repoapp\n\ngo 1.25.0\n",
		"go.sum":    "",
		"main.go":   "package main\n\nfunc main() {}\n",
		"README.md": "# repo app\n",
	}
	for rel, content := range files {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// emitJSON runs Analyze at level 1 and returns the v2 analysis.json bytes.
func emitJSON(t *testing.T, dir string, artifactText bool) []byte {
	t.Helper()
	opts := options.AnalysisOptions{
		InputPath: dir, AppName: "repoapp", Level: options.LevelSymbolTable,
		SkipTests: true, CacheDir: t.TempDir(), OutputDir: t.TempDir(), ArtifactText: artifactText,
	}
	app, err := core.New(opts).Analyze()
	if err != nil {
		t.Fatal(err)
	}
	identity, err := v2.ResolveIdentity("repoapp", nil)
	if err != nil {
		t.Fatal(err)
	}
	payload := v2emit.Emit(app, identity, dir, int(opts.Level), "test", 0, nil, nil)
	out, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestAnalyze_ArtifactsDefaultMetadataOnly(t *testing.T) {
	dir := repoProject(t)
	var doc struct {
		Application struct {
			Artifacts map[string]v2.Artifact `json:"artifacts"`
		} `json:"application"`
	}
	if err := json.Unmarshal(emitJSON(t, dir, false), &doc); err != nil {
		t.Fatal(err)
	}
	art, ok := doc.Application.Artifacts["README.md"]
	if !ok {
		t.Fatalf("README.md missing from artifacts: %v", doc.Application.Artifacts)
	}
	if art.ID != "can://repoapp/repoapp/artifact/README.md" {
		t.Errorf("artifact id = %q", art.ID)
	}
	if art.Format != "text" || art.Extraction != "none" || art.Source != "" {
		t.Errorf("default artifact should be metadata only: %+v", art)
	}
	if _, ok := doc.Application.Artifacts["main.go"]; ok {
		t.Errorf("a .go file became an artifact")
	}
}

func TestAnalyze_ArtifactTextCapturesSource(t *testing.T) {
	dir := repoProject(t)
	var doc struct {
		Application struct {
			Artifacts map[string]v2.Artifact `json:"artifacts"`
		} `json:"application"`
	}
	if err := json.Unmarshal(emitJSON(t, dir, true), &doc); err != nil {
		t.Fatal(err)
	}
	art := doc.Application.Artifacts["README.md"]
	if art.Source != "# repo app\n" || art.Extraction != "full" {
		t.Errorf("--artifact-text: source/extraction = %q/%q", art.Source, art.Extraction)
	}
	mod := doc.Application.Artifacts["go.mod"]
	if mod.Format != "gomod" || len(mod.Roles) != 1 || mod.Roles[0] != "dependency-manifest" {
		t.Errorf("go.mod artifact = %+v", mod)
	}
}

func TestAnalyze_ArtifactsReproducibleAcrossRuns(t *testing.T) {
	dir := repoProject(t)
	a := emitJSON(t, dir, false)
	b := emitJSON(t, dir, false)
	if string(a) != string(b) {
		t.Errorf("two runs over the same project differ")
	}
}
