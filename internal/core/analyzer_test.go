package core_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/codellm-devkit/codeanalyzer-go/internal/core"
	"github.com/codellm-devkit/codeanalyzer-go/internal/options"
	"github.com/codellm-devkit/codeanalyzer-go/internal/schema"
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
		if !sigs[e.Target] {
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
	if err := core.WriteOutput(sharedGreeterL2, outDir, "json"); err != nil {
		t.Fatalf("WriteOutput: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(outDir, "analysis.json"))
	if err != nil {
		t.Fatalf("reading analysis.json: %v", err)
	}
	var round schema.GoApplication
	if err := json.Unmarshal(data, &round); err != nil {
		t.Fatalf("JSON round-trip failed: %v", err)
	}
	if len(round.SymbolTable) == 0 {
		t.Error("round-tripped symbol table is empty")
	}
}

func TestWriteOutput_EmptyFormatDefaultsToJSON(t *testing.T) {
	outDir := t.TempDir()
	if err := core.WriteOutput(sharedGreeterL1, outDir, ""); err != nil {
		t.Fatalf("WriteOutput with empty format: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "analysis.json")); err != nil {
		t.Fatalf("analysis.json not written: %v", err)
	}
}

func TestWriteOutput_MsgpackNotImplemented(t *testing.T) {
	outDir := t.TempDir()
	if err := core.WriteOutput(sharedGreeterL1, outDir, "msgpack"); err == nil {
		t.Fatal("expected error for --format msgpack, got nil")
	}
}

func TestWriteOutput_UnknownFormatErrors(t *testing.T) {
	outDir := t.TempDir()
	if err := core.WriteOutput(sharedGreeterL1, outDir, "csv"); err == nil {
		t.Fatal("expected error for unknown format, got nil")
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
	if _, err := os.Stat(filepath.Join(cacheDir, "analysis_cache.json")); err != nil {
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
	data, err := os.ReadFile(filepath.Join(cacheDir, "analysis_cache.json"))
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
	cachePath := filepath.Join(cacheDir, "analysis_cache.json")
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

func allSignatures(app *schema.GoApplication) map[string]bool {
	sigs := map[string]bool{}
	for _, f := range app.SymbolTable {
		for sig := range f.Functions {
			sigs[sig] = true
		}
		for _, t := range f.Types {
			for sig := range t.Methods {
				sigs[sig] = true
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
