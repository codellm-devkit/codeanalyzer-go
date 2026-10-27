package core_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codellm-devkit/codeanalyzer-go/internal/core"
	"github.com/codellm-devkit/codeanalyzer-go/internal/options"
)

// TestAnalyze_Level4RunsSDG: -a 4 runs the L4 pass (SSA + VTA) after L3 and
// keeps its result; a clean project degrades nothing. Below -a 4 there is no
// L4 result.
func TestAnalyze_Level4RunsSDG(t *testing.T) {
	opts := options.AnalysisOptions{
		InputPath: greeterDir(t), Level: options.LevelSDG, SkipTests: true, CacheDir: t.TempDir(), Strict: true,
	}
	a := core.New(opts)
	if _, err := a.Analyze(); err != nil {
		t.Fatal(err)
	}
	if a.L4() == nil {
		t.Fatal("-a 4 produced no L4 result")
	}
	if len(a.L4().Degraded) != 0 {
		t.Errorf("degraded = %+v, want none", a.L4().Degraded)
	}
	if len(a.L3()) == 0 {
		t.Fatal("-a 4 produced no L3 results")
	}
	for k, r := range a.L3() {
		for _, n := range r.Nodes {
			if n.Stmt != nil {
				t.Fatalf("%v: node %s keeps its Stmt after -a 4", k, n.Key)
			}
		}
		if r.DefUse != nil || r.CallKeys != nil {
			t.Fatalf("%v keeps its L4 side tables after -a 4", k)
		}
	}

	opts.Level = options.LevelDataflow
	a3 := core.New(opts)
	if _, err := a3.Analyze(); err != nil {
		t.Fatal(err)
	}
	if a3.L4() != nil {
		t.Error("-a 3 ran the L4 pass")
	}
}

// TestAnalyze_Level4StrictFailsOnDegraded: an ill-typed package gets no SSA.
// By default -a 4 degrades and still succeeds; with --strict Analyze fails
// and the error names the degraded codes.
func TestAnalyze_Level4StrictFailsOnDegraded(t *testing.T) {
	opts := options.AnalysisOptions{
		InputPath: filepath.Join(testdataDir(), "typeerr"), Level: options.LevelSDG, SkipTests: true, CacheDir: t.TempDir(),
	}
	a := core.New(opts)
	if _, err := a.Analyze(); err != nil {
		t.Fatalf("non-strict -a 4 failed: %v", err)
	}
	if a.L4() == nil || len(a.L4().Degraded) == 0 {
		t.Fatalf("L4 = %+v, want a degraded result", a.L4())
	}

	opts.Strict = true
	_, err := core.New(opts).Analyze()
	if err == nil {
		t.Fatal("--strict -a 4 on a degraded project succeeded")
	}
	for _, code := range []string{"partial_types", "partial_ssa", "no_ssa"} {
		if !strings.Contains(err.Error(), code) {
			t.Errorf("error %q does not name %s", err, code)
		}
	}
}

// TestAnalyze_Level4MergesVTAEdges: at -a 4 the analyzer merges sdg.Run's
// VTA edges into the call graph and its VTA-only externals into
// app.Externals; -a 3 has neither.
func TestAnalyze_Level4MergesVTAEdges(t *testing.T) {
	opts := options.AnalysisOptions{
		InputPath: filepath.Join(testdataDir(), "dataflow"), AppName: "dataflow", Level: options.LevelSDG,
		SkipTests: true, CacheDir: t.TempDir(),
	}
	const src, dst = "example.com/dataflow/sdg.Buffered", "bytes.Buffer.Write"
	vtaEdge := func(level options.AnalysisLevel) (vta, ext bool) {
		opts.Level = level
		app, err := core.New(opts).Analyze()
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range app.CallGraph {
			if e.Source == src && e.Target == dst {
				vta = len(e.Provenance) == 1 && e.Provenance[0] == "vta"
			}
		}
		_, ext = app.Externals[dst]
		return vta, ext
	}
	if vta, ext := vtaEdge(options.LevelSDG); !vta || !ext {
		t.Errorf("-a 4: VTA edge %s -> %s = %v, external = %v; want both", src, dst, vta, ext)
	}
	if vta, ext := vtaEdge(options.LevelDataflow); vta || ext {
		t.Errorf("-a 3: VTA edge = %v, external = %v; want neither", vta, ext)
	}
}

// TestAnalyze_LibraryModels: --library-models is loaded into the L4 input. By
// default Greet's call of fmt.Sprintf has summary edges; the model
// fmt/Sprintf: [] (no flow) leaves it none. A missing or malformed file
// fails the run, naming the path.
func TestAnalyze_LibraryModels(t *testing.T) {
	summaries := func(t *testing.T, models string) int {
		t.Helper()
		a := core.New(options.AnalysisOptions{
			InputPath: greeterDir(t), Level: options.LevelSDG, SkipTests: true, CacheDir: t.TempDir(), LibraryModels: models,
		})
		if _, err := a.Analyze(); err != nil {
			t.Fatal(err)
		}
		n := 0
		for k, c := range a.L4().Callables {
			if strings.HasSuffix(k.Signature, "Greeter.Greet") {
				n += len(c.Summary)
			}
		}
		return n
	}
	if n := summaries(t, ""); n == 0 {
		t.Fatal("Greet has no summary edges by default")
	}
	dir := t.TempDir()
	good := filepath.Join(dir, "models.json")
	if err := os.WriteFile(good, []byte(`{"fmt/Sprintf": []}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if n := summaries(t, good); n != 0 {
		t.Errorf("fmt/Sprintf modelled with no flow: Greet has %d summary edges", n)
	}

	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte(`{"fmt/Sprintf": [`), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(dir, "absent.json"), bad} {
		_, err := core.New(options.AnalysisOptions{
			InputPath: greeterDir(t), Level: options.LevelSDG, SkipTests: true, CacheDir: t.TempDir(), LibraryModels: path,
		}).Analyze()
		if err == nil || !strings.Contains(err.Error(), path) {
			t.Errorf("%s: Analyze error = %v, want one naming the path", path, err)
		}
	}
}
