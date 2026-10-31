package core_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/codellm-devkit/codeanalyzer-go/internal/core"
	"github.com/codellm-devkit/codeanalyzer-go/internal/options"
	"github.com/codellm-devkit/codeanalyzer-go/internal/schema"
	v2 "github.com/codellm-devkit/codeanalyzer-go/internal/schema/v2"
)

// greeterDir returns the absolute path to testdata/greeter.
// Still used by the caching tests, which must run fresh analysis.
func greeterDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(testdataDir(), "greeter")
}

// ── Symbol table tests ────────────────────────────────────────────────────────

func TestSymbolTable_NonEmpty(t *testing.T) {
	if len(sharedGreeterL1.SymbolTable) == 0 {
		t.Fatal("symbol table is empty")
	}
}

func TestSymbolTable_PathKeysAreRelative(t *testing.T) {
	for key := range sharedGreeterL1.SymbolTable {
		if filepath.IsAbs(key) {
			t.Errorf("symbol_table key is absolute path: %s", key)
		}
	}
}

func TestSymbolTable_KnownType(t *testing.T) {
	const wantFile = "pkg/greeter/greeter.go"
	f, ok := sharedGreeterL1.SymbolTable[wantFile]
	if !ok {
		t.Fatalf("file %q not in symbol table; got keys: %v", wantFile, keys(sharedGreeterL1.SymbolTable))
	}
	if _, ok := f.Types["Greeter"]; !ok {
		t.Errorf("GoType 'Greeter' not found in %s", wantFile)
	}
}

func TestSymbolTable_KnownInterface(t *testing.T) {
	f := sharedGreeterL1.SymbolTable["pkg/greeter/greeter.go"]
	gt, ok := f.Types["Logger"]
	if !ok {
		t.Fatal("GoType 'Logger' not found")
	}
	if !gt.IsInterface {
		t.Error("Logger.is_interface should be true")
	}
}

func TestSymbolTable_StructFields(t *testing.T) {
	f := sharedGreeterL1.SymbolTable["pkg/greeter/greeter.go"]
	gt := f.Types["Greeter"]
	if len(gt.Fields) == 0 {
		t.Fatal("Greeter has no fields")
	}
	if gt.Fields[0].Name != "Prefix" {
		t.Errorf("expected field 'Prefix', got %q", gt.Fields[0].Name)
	}
	if _, hasJSON := gt.Fields[0].Tags["json"]; !hasJSON {
		t.Error("Greeter.Prefix missing json struct tag")
	}
}

func TestSymbolTable_CallSitesRecorded(t *testing.T) {
	f := sharedGreeterL1.SymbolTable["main.go"]
	var mainFn *schema.GoCallable
	for _, c := range f.Functions {
		c := c
		if c.Name == "main" {
			mainFn = &c
			break
		}
	}
	if mainFn == nil {
		t.Fatal("main function not found")
	}
	if len(mainFn.CallSites) == 0 {
		t.Error("main() has no recorded call sites")
	}
	for _, cs := range mainFn.CallSites {
		if cs.CalleeSignature != nil {
			t.Errorf("call site %q has callee_signature pre-filled during symbol-table build", cs.MethodName)
		}
	}
}

// ── Call graph tests ──────────────────────────────────────────────────────────

func TestCallGraph_NonEmpty(t *testing.T) {
	if len(sharedGreeterL2.CallGraph) == 0 {
		t.Fatal("call graph is empty")
	}
}

func TestCallGraph_NoDanglingEdges(t *testing.T) {
	sigs := allSignatures(sharedGreeterL2)
	for _, e := range sharedGreeterL2.CallGraph {
		if !sigs[e.Source] {
			t.Errorf("dangling edge source: %s", e.Source)
		}
		// A target is either an in-project callable or a recorded library callee.
		if _, ext := sharedGreeterL2.Externals[e.Target]; !sigs[e.Target] && !ext {
			t.Errorf("dangling edge target: %s", e.Target)
		}
	}
}

func TestCallGraph_Provenance(t *testing.T) {
	for _, e := range sharedGreeterL2.CallGraph {
		if len(e.Provenance) == 0 {
			t.Errorf("edge %s→%s has empty provenance", e.Source, e.Target)
		}
	}
}

func TestCallGraph_CallSitesBackfilled(t *testing.T) {
	f := sharedGreeterL2.SymbolTable["main.go"]
	for _, callable := range f.Functions {
		for _, cs := range callable.CallSites {
			if cs.CalleeSignature != nil && *cs.CalleeSignature == "" {
				t.Errorf("callable %s: call site %q has empty string callee_signature", callable.Signature, cs.MethodName)
			}
		}
	}
}

// ── JSON output tests ─────────────────────────────────────────────────────────

func TestWriteOutput_ValidJSON(t *testing.T) {
	outDir := t.TempDir()
	if err := core.WriteOutput(sharedGreeterL2, nil, nil, options.AnalysisOptions{OutputDir: outDir, Format: "json"}); err != nil {
		t.Fatalf("WriteOutput: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(outDir, "analysis.json"))
	if err != nil {
		t.Fatalf("reading analysis.json: %v", err)
	}
	// Output is the canonical v2 containment tree; round-trip into it.
	var round v2.Analysis
	if err := json.Unmarshal(data, &round); err != nil {
		t.Fatalf("JSON round-trip failed: %v", err)
	}
	if round.SchemaVersion != v2.SchemaVersion {
		t.Errorf("schema_version = %q, want %q", round.SchemaVersion, v2.SchemaVersion)
	}
	if len(round.Application.SymbolTable) == 0 {
		t.Error("round-tripped symbol table is empty")
	}
}

func TestWriteOutput_EmptyFormatDefaultsToJSON(t *testing.T) {
	outDir := t.TempDir()
	if err := core.WriteOutput(sharedGreeterL1, nil, nil, options.AnalysisOptions{OutputDir: outDir, Format: ""}); err != nil {
		t.Fatalf("WriteOutput with empty format: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "analysis.json")); err != nil {
		t.Fatalf("analysis.json not written: %v", err)
	}
}

func TestWriteOutput_MsgpackNotImplemented(t *testing.T) {
	outDir := t.TempDir()
	if err := core.WriteOutput(sharedGreeterL1, nil, nil, options.AnalysisOptions{OutputDir: outDir, Format: "msgpack"}); err == nil {
		t.Fatal("expected error for --format msgpack, got nil")
	}
}

func TestWriteOutput_UnknownFormatErrors(t *testing.T) {
	outDir := t.TempDir()
	if err := core.WriteOutput(sharedGreeterL1, nil, nil, options.AnalysisOptions{OutputDir: outDir, Format: "csv"}); err == nil {
		t.Fatal("expected error for unknown format, got nil")
	}
}

// TestWriteOutput_StreamsRenderJSONBytes: the streamed file and writer paths
// write exactly RenderJSON's bytes at -a 3 and -a 4 (the L4 param edges and
// degraded marker included), and Analyze has already released every L3
// node's AST reference (#27).
func TestWriteOutput_StreamsRenderJSONBytes(t *testing.T) {
	for _, level := range []options.AnalysisLevel{options.LevelDataflow, options.LevelSDG} {
		t.Run(fmt.Sprintf("a%d", level), func(t *testing.T) { streamsRenderJSONBytes(t, "dataflow", level) })
	}
	t.Run("a4-degraded", func(t *testing.T) { streamsRenderJSONBytes(t, "rangefunc", options.LevelSDG) })
}

func streamsRenderJSONBytes(t *testing.T, fixture string, level options.AnalysisLevel) {
	opts := options.AnalysisOptions{
		InputPath: filepath.Join(testdataDir(), fixture), AppName: fixture, Level: level,
		SkipTests: true, CacheDir: t.TempDir(), OutputDir: t.TempDir(),
	}
	a := core.New(opts)
	app, err := a.Analyze()
	if err != nil {
		t.Fatal(err)
	}
	for key, r := range a.L3() {
		for _, n := range r.Nodes {
			if n.Stmt != nil {
				t.Fatalf("%v: node %s still holds its AST after Analyze", key, n.Key)
			}
		}
	}
	want, err := core.RenderJSON(app, a.L3(), a.L4(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if level == options.LevelSDG {
		key := `"param_in":[`
		if fixture == "rangefunc" {
			key = `"degraded":[`
		}
		if !bytes.Contains(want, []byte(key)) {
			t.Fatalf("-a 4 JSON of %s has no %s", fixture, key)
		}
	}
	var buf bytes.Buffer
	if err := core.StreamJSON(&buf, app, a.L3(), a.L4(), opts); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Error("StreamJSON bytes differ from RenderJSON")
	}
	if err := core.WriteOutput(app, a.L3(), a.L4(), opts); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(opts.OutputDir, "analysis.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Error("analysis.json differs from RenderJSON")
	}
	if ents, _ := os.ReadDir(opts.OutputDir); len(ents) != 1 {
		t.Errorf("output dir holds %d entries, want only analysis.json", len(ents))
	}
}

// ── Caching tests ─────────────────────────────────────────────────────────────
// These tests must run their own analysis to exercise the caching machinery.

func TestCaching_SecondRunReuses(t *testing.T) {
	dir := greeterDir(t)
	cacheDir := t.TempDir()
	opts := options.AnalysisOptions{
		InputPath: dir,
		OutputDir: t.TempDir(),
		Level:     options.LevelCallGraph,
		SkipTests: true,
		CacheDir:  cacheDir,
	}
	app1, err := core.New(opts).Analyze()
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	app2, err := core.New(opts).Analyze()
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(app2.SymbolTable) == 0 {
		t.Error("second run returned empty symbol table")
	}
	if len(app2.SymbolTable) != len(app1.SymbolTable) {
		t.Errorf("symbol table key count changed between runs: %d → %d",
			len(app1.SymbolTable), len(app2.SymbolTable))
	}
}

func TestCaching_CacheFileWritten(t *testing.T) {
	cacheDir := t.TempDir()
	opts := options.AnalysisOptions{
		InputPath: greeterDir(t),
		Level:     options.LevelSymbolTable,
		SkipTests: true,
		CacheDir:  cacheDir,
	}
	if _, err := core.New(opts).Analyze(); err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if _, err := os.Stat(identityCacheFile(t, cacheDir)); err != nil {
		t.Fatalf("analysis_cache.json not written to CacheDir: %v", err)
	}
}

func TestCaching_CacheContentsRoundTrip(t *testing.T) {
	cacheDir := t.TempDir()
	opts := options.AnalysisOptions{
		InputPath: greeterDir(t),
		Level:     options.LevelSymbolTable,
		SkipTests: true,
		CacheDir:  cacheDir,
	}
	app, err := core.New(opts).Analyze()
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	data, err := os.ReadFile(identityCacheFile(t, cacheDir))
	if err != nil {
		t.Fatalf("reading analysis_cache.json: %v", err)
	}
	var cached schema.GoApplication
	if err := json.Unmarshal(data, &cached); err != nil {
		t.Fatalf("cache JSON round-trip failed: %v", err)
	}
	if len(cached.SymbolTable) != len(app.SymbolTable) {
		t.Errorf("cache symbol table key count %d != in-memory %d",
			len(cached.SymbolTable), len(app.SymbolTable))
	}
}

func TestCaching_EagerForcesRebuild(t *testing.T) {
	cacheDir := t.TempDir()
	opts := options.AnalysisOptions{
		InputPath: greeterDir(t),
		Level:     options.LevelSymbolTable,
		SkipTests: true,
		CacheDir:  cacheDir,
	}
	if _, err := core.New(opts).Analyze(); err != nil {
		t.Fatalf("first run: %v", err)
	}
	cachePath := identityCacheFile(t, cacheDir)
	info1, err := os.Stat(cachePath)
	if err != nil {
		t.Fatalf("cache not written after first run: %v", err)
	}

	// Backdate the cache file so the mtime delta is unambiguous — no sleep needed.
	past := info1.ModTime().Add(-time.Second)
	if err := os.Chtimes(cachePath, past, past); err != nil {
		t.Fatalf("backdating cache mtime: %v", err)
	}

	opts.Eager = true
	if _, err := core.New(opts).Analyze(); err != nil {
		t.Fatalf("eager run: %v", err)
	}
	info2, err := os.Stat(cachePath)
	if err != nil {
		t.Fatalf("cache not found after eager run: %v", err)
	}
	if !info2.ModTime().After(past) {
		t.Errorf("analysis_cache.json mtime did not advance on eager=true run: %v vs %v",
			past, info2.ModTime())
	}
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// allSignatures collects every callable signature, closures included (a closure
// is a call-graph edge source).
func allSignatures(app *schema.GoApplication) map[string]bool {
	sigs := map[string]bool{}
	var add func(sig string, c schema.GoCallable)
	add = func(sig string, c schema.GoCallable) {
		sigs[sig] = true
		for _, ic := range c.InnerCallables {
			add(ic.Signature, ic)
		}
	}
	for _, f := range app.SymbolTable {
		for sig, c := range f.Functions {
			add(sig, c)
		}
		for _, t := range f.Types {
			for sig, c := range t.Methods {
				add(sig, c)
			}
		}
	}
	return sigs
}

func keys[K comparable, V any](m map[K]V) []K {
	out := make([]K, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func identityCacheFile(t *testing.T, dir string) string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "identity-v2-*", "analysis_cache.json"))
	if err != nil || len(files) != 1 {
		t.Fatalf("identity cache files: %v %v", files, err)
	}
	return files[0]
}
func TestCaching_NamespaceIsolation(t *testing.T) {
	dir := t.TempDir()
	for _, ns := range []string{"team-a", "team-b"} {
		opts := options.AnalysisOptions{InputPath: greeterDir(t), AppName: "greeter", Namespace: &ns, Level: options.LevelSymbolTable, SkipTests: true, CacheDir: dir}
		if _, err := core.New(opts).Analyze(); err != nil {
			t.Fatal(err)
		}
	}
	files, err := filepath.Glob(filepath.Join(dir, "identity-v2-*", "analysis_cache.json"))
	if err != nil || len(files) != 2 {
		t.Fatalf("cache scopes: %v %v", files, err)
	}
}

func TestAnalyzeInvalidNamespaceReadOnly(t *testing.T) {
	ns := ""
	dir := t.TempDir()
	opts := options.AnalysisOptions{InputPath: greeterDir(t), AppName: "greeter", Namespace: &ns, Level: options.LevelSymbolTable, CacheDir: dir}
	if _, err := core.New(opts).Analyze(); err == nil {
		t.Fatal("accepted empty namespace")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("cache changed: %v %v", entries, err)
	}
}

func TestWriteOutputInvalidIdentityPreservesFilesystem(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprint(existing), func(t *testing.T) {
			root := t.TempDir()
			dst := filepath.Join(root, "missing", "output")
			if existing {
				if err := os.MkdirAll(dst, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dst, "analysis.json"), []byte("protected"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			ns := ""
			opts := options.AnalysisOptions{InputPath: greeterDir(t), AppName: "greeter", Namespace: &ns, OutputDir: dst}
			if err := core.WriteOutput(&schema.GoApplication{}, nil, nil, opts); err == nil {
				t.Fatal("accepted invalid identity")
			}
			if !existing {
				entries, err := os.ReadDir(root)
				if err != nil || len(entries) != 0 {
					t.Fatalf("created destination: %v %v", entries, err)
				}
			} else {
				entries, err := os.ReadDir(dst)
				if err != nil || len(entries) != 1 {
					t.Fatalf("changed destination: %v %v", entries, err)
				}
				b, err := os.ReadFile(filepath.Join(dst, "analysis.json"))
				if err != nil || string(b) != "protected" {
					t.Fatalf("changed output: %q %v", b, err)
				}
			}
		})
	}
}
