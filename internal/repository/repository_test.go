package repository

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/codellm-devkit/codeanalyzer-go/internal/schema"
)

// writeTree creates the files under root (path → content). A NUL in content
// makes a file binary.
func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// appWithImports is a symbol-table stub carrying the given import paths in one file.
func appWithImports(paths ...string) *schema.GoApplication {
	imps := make([]schema.GoImport, 0, len(paths))
	for _, p := range paths {
		imps = append(imps, schema.GoImport{Module: p})
	}
	return &schema.GoApplication{SymbolTable: map[string]schema.GoFile{"main.go": {Imports: imps}}}
}

const goMod = `module example.com/app

go 1.25.0

require (
	cloud.google.com/go v0.1.0
	cloud.google.com/go/auth v0.2.0
	github.com/gorilla/mux v1.8.0
	golang.org/x/text v0.14.0 // indirect
)
`

// go.sum: mux has both lines for its version (locked); auth too; cloud.google.com/go
// has only its h1 line (not locked); x/text has only its /go.mod line (not locked).
const goSum = `cloud.google.com/go v0.1.0 h1:AAA=
cloud.google.com/go/auth v0.2.0 h1:BBB=
cloud.google.com/go/auth v0.2.0/go.mod h1:CCC=
github.com/gorilla/mux v1.8.0 h1:DDD=
github.com/gorilla/mux v1.8.0/go.mod h1:EEE=
golang.org/x/text v0.14.0/go.mod h1:FFF=
`

func TestFacts_DependenciesLocksAndImports(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"go.mod": goMod, "go.sum": goSum, "main.go": "package main\n"})

	facts, err := Facts(root, appWithImports(
		"github.com/gorilla/mux",
		"cloud.google.com/go/auth/credentials", // longest prefix: cloud.google.com/go/auth
		"cloud.google.com/go/storage",          // only cloud.google.com/go matches
		"fmt",                                  // stdlib: no requirement
	), Options{})
	if err != nil {
		t.Fatal(err)
	}

	want := []schema.GoDependency{
		{Name: "cloud.google.com/go", Ecosystem: "golang", Spec: "v0.1.0", Kind: "runtime", Direct: true,
			DeclaredIn: "go.mod", ProvidesImports: []string{"cloud.google.com/go/storage"}, Prov: []string{"declared"}},
		{Name: "cloud.google.com/go/auth", Ecosystem: "golang", Spec: "v0.2.0", Kind: "runtime", Direct: true,
			LockedVersion: "v0.2.0", DeclaredIn: "go.mod", ProvidesImports: []string{"cloud.google.com/go/auth/credentials"},
			Prov: []string{"declared", "lockfile"}},
		{Name: "github.com/gorilla/mux", Ecosystem: "golang", Spec: "v1.8.0", Kind: "runtime", Direct: true,
			LockedVersion: "v1.8.0", DeclaredIn: "go.mod", ProvidesImports: []string{"github.com/gorilla/mux"},
			Prov: []string{"declared", "lockfile"}},
		{Name: "golang.org/x/text", Ecosystem: "golang", Spec: "v0.14.0", Kind: "runtime", Direct: false,
			DeclaredIn: "go.mod", Prov: []string{"declared"}},
	}
	if !reflect.DeepEqual(facts.Dependencies, want) {
		t.Errorf("dependencies:\n got  %+v\n want %+v", facts.Dependencies, want)
	}
}

func TestFacts_ArtifactsClassifiedAndSkipped(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"go.mod":              goMod,
		"go.sum":              goSum,
		"main.go":             "package main\n",
		"main_test.go":        "package main\n",
		"Dockerfile":          "FROM scratch\n",
		"config/app.yaml":     "a: 1\n",
		"config/settings.env": "KEY=value\n",
		"blob.dat":            "ab\x00cd",
		".git/HEAD":           "ref: refs/heads/main\n",
		"vendor/x/y.txt":      "vendored\n",
	})

	facts, err := Facts(root, appWithImports(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]schema.GoArtifact{}
	for p, a := range facts.Artifacts {
		got[p] = a
	}
	wantFormats := map[string]string{
		"go.mod":              "gomod",
		"go.sum":              "gosum",
		"Dockerfile":          "dockerfile",
		"config/app.yaml":     "yaml",
		"config/settings.env": "text",
		"blob.dat":            "binary",
	}
	if len(got) != len(wantFormats) {
		t.Errorf("artifacts = %v, want exactly %v", keys(got), keys(wantFormats))
	}
	for p, f := range wantFormats {
		a, ok := got[p]
		if !ok {
			t.Errorf("missing artifact %s", p)
			continue
		}
		if a.Format != f {
			t.Errorf("%s format = %q, want %q", p, a.Format, f)
		}
		if a.Extraction != "none" || a.Source != "" {
			t.Errorf("%s captured source without --artifact-text: %+v", p, a)
		}
		if len(a.SHA256) != 64 {
			t.Errorf("%s sha256 = %q, want 64 hex chars", p, a.SHA256)
		}
	}
	if r := got["go.mod"].Roles; !reflect.DeepEqual(r, []string{"dependency-manifest"}) {
		t.Errorf("go.mod roles = %v", r)
	}
	if r := got["go.sum"].Roles; !reflect.DeepEqual(r, []string{"lockfile"}) {
		t.Errorf("go.sum roles = %v", r)
	}
}

func TestDiscover_CaptureTextOnlyForText(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"go.mod": goMod, "blob.dat": "ab\x00cd"})

	on, err := Discover(root, Options{CaptureText: true})
	if err != nil {
		t.Fatal(err)
	}
	if on["go.mod"].Source != goMod || on["go.mod"].Extraction != "full" {
		t.Errorf("go.mod with --artifact-text: source/extraction = %q/%q", on["go.mod"].Source, on["go.mod"].Extraction)
	}
	if on["blob.dat"].Source != "" || on["blob.dat"].Extraction != "none" {
		t.Errorf("binary captured source: %+v", on["blob.dat"])
	}
}

func TestDiscover_ExcludesOwnOutputsAndCache(t *testing.T) {
	root := t.TempDir()
	cache := filepath.Join(root, "cache")
	writeTree(t, root, map[string]string{
		"README.md":           "# app\n",
		"analysis.json":       "{}",
		"graph.cypher":        "CREATE;",
		"cache/entry.bin":     "x\x00",
		"notes/analysis.json": "{}", // not the output dir: kept
	})

	got, err := Discover(root, Options{OutputDir: root, CacheDir: cache})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got["analysis.json"]; ok {
		t.Errorf("own output analysis.json was captured")
	}
	if _, ok := got["graph.cypher"]; ok {
		t.Errorf("own output graph.cypher was captured")
	}
	if _, ok := got["cache/entry.bin"]; ok {
		t.Errorf("cache subtree was captured")
	}
	if _, ok := got["notes/analysis.json"]; !ok {
		t.Errorf("unrelated notes/analysis.json was wrongly excluded")
	}
	if _, ok := got["README.md"]; !ok {
		t.Errorf("README.md missing")
	}
}

func TestFacts_NoGoModMeansArtifactsOnly(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"Dockerfile": "FROM scratch\n"})

	facts, err := Facts(root, appWithImports("fmt"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(facts.Dependencies) != 0 {
		t.Errorf("dependencies without go.mod: %+v", facts.Dependencies)
	}
	if _, ok := facts.Artifacts["Dockerfile"]; !ok {
		t.Errorf("Dockerfile artifact missing")
	}
}

func TestParseGoSum_OnlyFullPairsLock(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"go.sum": goSum})
	got := parseGoSum(filepath.Join(root, "go.sum"))
	if !got["github.com/gorilla/mux"]["v1.8.0"] {
		t.Errorf("mux v1.8.0 has both lines; want locked")
	}
	if got["cloud.google.com/go"]["v0.1.0"] {
		t.Errorf("cloud.google.com/go has only h1; want not locked")
	}
	if got["golang.org/x/text"]["v0.14.0"] {
		t.Errorf("golang.org/x/text has only /go.mod; want not locked")
	}
	if n := len(parseGoSum(filepath.Join(root, "absent"))); n != 0 {
		t.Errorf("missing go.sum locked %d modules", n)
	}
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
