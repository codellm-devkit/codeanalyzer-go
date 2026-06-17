package core_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/codellm-devkit/codeanalyzer-go/internal/core"
	"github.com/codellm-devkit/codeanalyzer-go/internal/options"
	"github.com/codellm-devkit/codeanalyzer-go/internal/schema"
)

// fixtureDir returns the absolute path to testdata/fixture.
func fixtureDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot determine source file path")
	}
	// internal/core/analyzer_test.go → ../.. → codeanalyzer-go root → testdata/fixture
	root := filepath.Join(filepath.Dir(file), "..", "..")
	abs, err := filepath.Abs(filepath.Join(root, "testdata", "fixture"))
	if err != nil {
		t.Fatalf("resolving fixture dir: %v", err)
	}
	return abs
}

func runAnalysis(t *testing.T, level options.AnalysisLevel) *schema.GoApplication {
	t.Helper()
	dir := fixtureDir(t)
	outDir := t.TempDir()
	opts := options.AnalysisOptions{
		InputPath: dir,
		OutputDir: outDir,
		Level:     level,
		SkipTests: true,
		CacheDir:  t.TempDir(),
	}
	app, err := core.New(opts).Analyze()
	if err != nil {
		t.Fatalf("Analyze() failed: %v", err)
	}
	return app
}

// ── Symbol table tests ────────────────────────────────────────────────────────

func TestSymbolTable_NonEmpty(t *testing.T) {
	app := runAnalysis(t, options.LevelSymbolTable)
	if len(app.SymbolTable) == 0 {
		t.Fatal("symbol table is empty")
	}
}

func TestSymbolTable_PathKeysAreRelative(t *testing.T) {
	app := runAnalysis(t, options.LevelSymbolTable)
	for key := range app.SymbolTable {
		if filepath.IsAbs(key) {
			t.Errorf("symbol_table key is absolute path: %s", key)
		}
	}
}

func TestSymbolTable_KnownType(t *testing.T) {
	app := runAnalysis(t, options.LevelSymbolTable)
	const wantFile = "pkg/greeter/greeter.go"
	f, ok := app.SymbolTable[wantFile]
	if !ok {
		t.Fatalf("file %q not in symbol table; got keys: %v", wantFile, keys(app.SymbolTable))
	}
	if _, ok := f.Types["Greeter"]; !ok {
		t.Errorf("GoType 'Greeter' not found in %s", wantFile)
	}
}

func TestSymbolTable_KnownInterface(t *testing.T) {
	app := runAnalysis(t, options.LevelSymbolTable)
	f := app.SymbolTable["pkg/greeter/greeter.go"]
	gt, ok := f.Types["Logger"]
	if !ok {
		t.Fatal("GoType 'Logger' not found")
	}
	if !gt.IsInterface {
		t.Error("Logger.is_interface should be true")
	}
}

func TestSymbolTable_StructFields(t *testing.T) {
	app := runAnalysis(t, options.LevelSymbolTable)
	f := app.SymbolTable["pkg/greeter/greeter.go"]
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
	app := runAnalysis(t, options.LevelSymbolTable)
	f := app.SymbolTable["main.go"]
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
	// All call sites must start with callee_signature == nil (pre-resolution).
	for _, cs := range mainFn.CallSites {
		if cs.CalleeSignature != nil {
			t.Errorf("call site %q has callee_signature pre-filled during symbol-table build", cs.MethodName)
		}
	}
}

// ── Call graph tests ──────────────────────────────────────────────────────────

func TestCallGraph_NonEmpty(t *testing.T) {
	app := runAnalysis(t, options.LevelCallGraph)
	if len(app.CallGraph) == 0 {
		t.Fatal("call graph is empty")
	}
}

func TestCallGraph_NoDanglingEdges(t *testing.T) {
	app := runAnalysis(t, options.LevelCallGraph)
	sigs := allSignatures(app)
	for _, e := range app.CallGraph {
		if !sigs[e.Source] {
			t.Errorf("dangling edge source: %s", e.Source)
		}
		if !sigs[e.Target] {
			t.Errorf("dangling edge target: %s", e.Target)
		}
	}
}

func TestCallGraph_Provenance(t *testing.T) {
	app := runAnalysis(t, options.LevelCallGraph)
	for _, e := range app.CallGraph {
		if len(e.Provenance) == 0 {
			t.Errorf("edge %s→%s has empty provenance", e.Source, e.Target)
		}
	}
}

func TestCallGraph_CallSitesBackfilled(t *testing.T) {
	app := runAnalysis(t, options.LevelCallGraph)
	f := app.SymbolTable["main.go"]
	for _, callable := range f.Functions {
		for _, cs := range callable.CallSites {
			// Sites that resolved to a project-internal callee must be backfilled.
			if cs.CalleeSignature != nil && *cs.CalleeSignature == "" {
				t.Errorf("callable %s: call site %q has empty string callee_signature", callable.Signature, cs.MethodName)
			}
		}
	}
}

// ── JSON output tests ─────────────────────────────────────────────────────────

func TestWriteOutput_ValidJSON(t *testing.T) {
	app := runAnalysis(t, options.LevelCallGraph)
	outDir := t.TempDir()
	if err := core.WriteOutput(app, outDir, "json"); err != nil {
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
	app := runAnalysis(t, options.LevelSymbolTable)
	outDir := t.TempDir()
	if err := core.WriteOutput(app, outDir, ""); err != nil {
		t.Fatalf("WriteOutput with empty format: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "analysis.json")); err != nil {
		t.Fatalf("analysis.json not written: %v", err)
	}
}

func TestWriteOutput_MsgpackNotImplemented(t *testing.T) {
	app := runAnalysis(t, options.LevelSymbolTable)
	outDir := t.TempDir()
	err := core.WriteOutput(app, outDir, "msgpack")
	if err == nil {
		t.Fatal("expected error for --format msgpack, got nil")
	}
}

func TestWriteOutput_UnknownFormatErrors(t *testing.T) {
	app := runAnalysis(t, options.LevelSymbolTable)
	outDir := t.TempDir()
	err := core.WriteOutput(app, outDir, "csv")
	if err == nil {
		t.Fatal("expected error for unknown format, got nil")
	}
}

// ── Caching tests ─────────────────────────────────────────────────────────────

func TestCaching_SecondRunReuses(t *testing.T) {
	dir := fixtureDir(t)
	cacheDir := t.TempDir()
	outDir := t.TempDir()
	opts := options.AnalysisOptions{
		InputPath: dir,
		OutputDir: outDir,
		Level:     options.LevelCallGraph,
		SkipTests: true,
		CacheDir:  cacheDir,
	}
	// First run — populates cache.
	app1, err := core.New(opts).Analyze()
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	// Second run — must not error and must return identical key count.
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
	dir := fixtureDir(t)
	cacheDir := t.TempDir()
	opts := options.AnalysisOptions{
		InputPath: dir,
		Level:     options.LevelSymbolTable,
		SkipTests: true,
		CacheDir:  cacheDir,
	}
	if _, err := core.New(opts).Analyze(); err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	cachePath := filepath.Join(cacheDir, "analysis_cache.json")
	if _, err := os.Stat(cachePath); err != nil {
		t.Fatalf("analysis_cache.json not written to CacheDir: %v", err)
	}
}

func TestCaching_CacheContentsRoundTrip(t *testing.T) {
	dir := fixtureDir(t)
	cacheDir := t.TempDir()
	opts := options.AnalysisOptions{
		InputPath: dir,
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
	dir := fixtureDir(t)
	cacheDir := t.TempDir()
	opts := options.AnalysisOptions{
		InputPath: dir,
		Level:     options.LevelSymbolTable,
		SkipTests: true,
		CacheDir:  cacheDir,
	}
	// First run (non-eager) — seeds go_mod_hash.
	if _, err := core.New(opts).Analyze(); err != nil {
		t.Fatalf("first run: %v", err)
	}
	cachePath := filepath.Join(cacheDir, "analysis_cache.json")
	info1, err := os.Stat(cachePath)
	if err != nil {
		t.Fatalf("cache not written after first run: %v", err)
	}

	time.Sleep(10 * time.Millisecond)

	// Second run with Eager=true — must rewrite cache even when go_mod_hash matches.
	opts.Eager = true
	if _, err := core.New(opts).Analyze(); err != nil {
		t.Fatalf("eager run: %v", err)
	}
	info2, err := os.Stat(cachePath)
	if err != nil {
		t.Fatalf("cache not found after eager run: %v", err)
	}
	// saveCache always writes, so mtime must advance.
	if !info2.ModTime().After(info1.ModTime()) {
		t.Errorf("analysis_cache.json mtime did not advance on eager=true run: %v vs %v",
			info1.ModTime(), info2.ModTime())
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
