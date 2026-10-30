package main

// CLI integration tests.  These call rootCmd().Execute() directly (same
// package, so the unexported function is accessible) with controlled args and
// capture cobra's output buffer.  No subprocess or binary required.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// cliTestdataDir returns the absolute path to the repo-level testdata directory.
func cliTestdataDir() string {
	_, thisFile, _, _ := runtime.Caller(0)
	abs, _ := filepath.Abs(filepath.Join(filepath.Dir(thisFile), "..", "..", "testdata"))
	return abs
}

// runCmd executes rootCmd with the given args and returns (stdout, stderr, error).
func runCmd(args ...string) (stdout, stderr string, err error) {
	cmd := rootCmd()
	var outBuf, errBuf bytes.Buffer
	cmd.SetOut(&outBuf)
	cmd.SetErr(&errBuf)
	cmd.SetArgs(args)
	err = cmd.Execute()
	return outBuf.String(), errBuf.String(), err
}

// ── Flag validation ───────────────────────────────────────────────────────────

func TestRootCmd_MissingInputReturnsError(t *testing.T) {
	_, _, err := runCmd()
	if err == nil {
		t.Fatal("expected error when --input is missing, got nil")
	}
	if !strings.Contains(err.Error(), "required") {
		t.Errorf("error should mention 'required'; got %q", err.Error())
	}
}

func TestRootCmd_NonExistentInputReturnsError(t *testing.T) {
	_, _, err := runCmd("--input", filepath.Join(t.TempDir(), "does_not_exist"))
	if err == nil {
		t.Fatal("expected error for non-existent --input path, got nil")
	}
}

func TestRootCmd_UnknownFormatReturnsError(t *testing.T) {
	td := cliTestdataDir()
	_, _, err := runCmd("--input", filepath.Join(td, "greeter"), "--format", "csv")
	if err == nil {
		t.Fatal("expected error for unknown --format value, got nil")
	}
}

func TestRootCmd_UnknownEmitReturnsError(t *testing.T) {
	td := cliTestdataDir()
	_, _, err := runCmd("--input", filepath.Join(td, "greeter"), "--emit", "bogus")
	if err == nil {
		t.Fatal("expected error for unknown --emit value, got nil")
	}
}

func TestRootCmd_EmitNeo4jWritesGraphCypher(t *testing.T) {
	// --emit neo4j with no --neo4j-uri writes a self-contained graph.cypher
	// snapshot into the output dir (M6).
	td := cliTestdataDir()
	out := t.TempDir()
	stdout, _, err := runCmd("--input", filepath.Join(td, "multipackage"), "--emit", "neo4j", "--output", out)
	if err != nil {
		t.Fatalf("--emit neo4j should succeed; got %v", err)
	}
	data, err := os.ReadFile(filepath.Join(out, "graph.cypher"))
	if err != nil {
		t.Fatalf("graph.cypher not written: %v", err)
	}
	script := string(data)
	for _, want := range []string{"CREATE CONSTRAINT", "DETACH DELETE", "MERGE (n:", "GO_CALLS"} {
		if !strings.Contains(script, want) {
			t.Errorf("graph.cypher missing %q", want)
		}
	}
	if !strings.Contains(stdout, "graph.cypher") {
		t.Errorf("expected stdout to report the written path; got %q", stdout)
	}
}

func TestRootCmd_EmitNeo4jRejectsExplicitAnalysisLevel(t *testing.T) {
	// --emit neo4j is always full-depth; combining it with -a is a flag error.
	td := cliTestdataDir()
	_, _, err := runCmd("--input", filepath.Join(td, "greeter"), "--emit", "neo4j", "-a", "1")
	if err == nil {
		t.Fatal("expected a flag error for --emit neo4j -a 1, got nil")
	}
	if !strings.Contains(err.Error(), "does not apply to --emit neo4j") {
		t.Errorf("error should explain the level gate; got %q", err.Error())
	}
}

func TestRootCmd_EmitSchemaWritesContractWithoutInput(t *testing.T) {
	// --emit schema is a static contract: it needs no --input and writes
	// schema.neo4j.json to the output dir (M1).
	out := t.TempDir()
	stdout, _, err := runCmd("--emit", "schema", "--output", out)
	if err != nil {
		t.Fatalf("--emit schema should succeed without --input; got %v", err)
	}

	path := filepath.Join(out, "schema.neo4j.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("schema.neo4j.json not written: %v", err)
	}
	if !strings.Contains(stdout, "schema.neo4j.json") {
		t.Errorf("expected stdout to report the written path; got %q", stdout)
	}

	// It must parse and carry the schema version + a GO_-prefixed vocabulary.
	var doc struct {
		SchemaVersion string `json:"schema_version"`
		RelPrefix     string `json:"rel_prefix"`
		Nodes         []struct {
			Label string `json:"label"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("schema.neo4j.json does not parse: %v", err)
	}
	if doc.SchemaVersion != "2.0.0" {
		t.Errorf("schema_version = %q, want 2.0.0", doc.SchemaVersion)
	}
	if doc.RelPrefix != "GO_" {
		t.Errorf("rel_prefix = %q, want GO_", doc.RelPrefix)
	}
	if len(doc.Nodes) == 0 {
		t.Error("schema document lists no node families")
	}
}

// ── --version ────────────────────────────────────────────────────────────────

func TestRootCmd_VersionFlag(t *testing.T) {
	out, _, err := runCmd("--version")
	if err != nil {
		t.Fatalf("--version returned unexpected error: %v", err)
	}
	if !strings.Contains(out, "cango") {
		t.Errorf("--version output should contain 'cango'; got %q", out)
	}
	if !strings.Contains(out, version) {
		t.Errorf("--version output should contain version %q; got %q", version, out)
	}
}

// ── --output writes analysis.json ────────────────────────────────────────────

func TestRootCmd_OutputDirWritesFile(t *testing.T) {
	td := cliTestdataDir()
	outDir := t.TempDir()

	_, _, err := runCmd(
		"--input", filepath.Join(td, "greeter"),
		"--output", outDir,
		"--cache-dir", t.TempDir(),
	)
	if err != nil {
		t.Fatalf("command failed: %v", err)
	}

	analysisPath := filepath.Join(outDir, "analysis.json")
	if _, statErr := os.Stat(analysisPath); statErr != nil {
		t.Fatalf("analysis.json not created in output dir: %v", statErr)
	}
}

func TestRootCmd_NoOutputWritesToStdout(t *testing.T) {
	td := cliTestdataDir()

	out, _, err := runCmd(
		"--input", filepath.Join(td, "greeter"),
		"--cache-dir", t.TempDir(),
	)
	if err != nil {
		t.Fatalf("command failed: %v", err)
	}
	if out == "" {
		t.Fatal("expected JSON on stdout when --output is omitted, got empty string")
	}
	var v interface{}
	if jsonErr := json.Unmarshal([]byte(out), &v); jsonErr != nil {
		t.Errorf("stdout is not valid JSON: %v\noutput: %s", jsonErr, out)
	}
}

// ── --analysis-level ─────────────────────────────────────────────────────────

func TestRootCmd_Level1ProducesNoCallGraph(t *testing.T) {
	td := cliTestdataDir()

	out, _, err := runCmd(
		"--input", filepath.Join(td, "greeter"),
		"--analysis-level", "1",
		"--cache-dir", t.TempDir(),
	)
	if err != nil {
		t.Fatalf("command failed: %v", err)
	}

	// The default is now the v2 shape, where call_graph lives under application (not top-level).
	var result struct {
		Application struct {
			CallGraph []interface{} `json:"call_graph"`
		} `json:"application"`
	}
	if jsonErr := json.Unmarshal([]byte(out), &result); jsonErr != nil {
		t.Fatalf("stdout is not valid JSON: %v", jsonErr)
	}
	if len(result.Application.CallGraph) != 0 {
		t.Errorf("level 1 should produce no call graph edges; got %d", len(result.Application.CallGraph))
	}
}

func TestRootCmd_Level2ProducesCallGraph(t *testing.T) {
	td := cliTestdataDir()

	out, _, err := runCmd(
		"--input", filepath.Join(td, "greeter"),
		"--analysis-level", "2",
		"--cache-dir", t.TempDir(),
	)
	if err != nil {
		t.Fatalf("command failed: %v", err)
	}

	// The default is now the v2 shape, where call_graph lives under application (not top-level).
	var result struct {
		Application struct {
			CallGraph []interface{} `json:"call_graph"`
		} `json:"application"`
	}
	if jsonErr := json.Unmarshal([]byte(out), &result); jsonErr != nil {
		t.Fatalf("stdout is not valid JSON: %v", jsonErr)
	}
	if len(result.Application.CallGraph) == 0 {
		t.Error("level 2 should produce call graph edges; got none")
	}
}

// ── schema output (always canonical v2) ─────────────────────────────────────────

// The analyzer emits only the canonical v2 containment tree. This asserts the
// envelope shape and identity anchor on the default (and only) output path.
func TestRootCmd_EmitsCanonicalV2(t *testing.T) {
	td := cliTestdataDir()
	out, _, err := runCmd("--input", filepath.Join(td, "greeter"), "--cache-dir", t.TempDir())
	if err != nil {
		t.Fatalf("command failed: %v", err)
	}
	var v struct {
		SchemaVersion string `json:"schema_version"`
		Language      string `json:"language"`
		Application   struct {
			ID string `json:"id"`
		} `json:"application"`
	}
	if jsonErr := json.Unmarshal([]byte(out), &v); jsonErr != nil {
		t.Fatalf("stdout is not valid JSON: %v", jsonErr)
	}
	if v.SchemaVersion != "2.0.0" {
		t.Errorf("schema_version = %q, want 2.0.0", v.SchemaVersion)
	}
	if v.Language != "go" {
		t.Errorf("language = %q, want go", v.Language)
	}
	if v.Application.ID != "can://greeter/greeter" {
		t.Errorf("application.id = %q, want can://greeter/greeter", v.Application.ID)
	}
}

// ── --skip-tests ─────────────────────────────────────────────────────────────

func TestRootCmd_SkipTestsFalseIncludesTestFiles(t *testing.T) {
	td := cliTestdataDir()

	out, _, err := runCmd(
		"--input", filepath.Join(td, "multipackage"),
		"--skip-tests=false",
		"--cache-dir", t.TempDir(),
	)
	if err != nil {
		t.Fatalf("command failed: %v", err)
	}

	if !strings.Contains(out, "server_test.go") {
		t.Error("--skip-tests=false: server_test.go should appear in JSON output")
	}
}

// ── --target-files ────────────────────────────────────────────────────────────

func TestRootCmd_TargetFilesRestrictsOutput(t *testing.T) {
	td := cliTestdataDir()
	serverFile := filepath.Join(td, "multipackage", "server", "server.go")

	out, _, err := runCmd(
		"--input", filepath.Join(td, "multipackage"),
		"--target-files", serverFile,
		"--cache-dir", t.TempDir(),
	)
	if err != nil {
		t.Fatalf("command failed: %v", err)
	}

	if strings.Contains(out, `"worker/worker.go"`) {
		t.Error("--target-files: worker/worker.go should not appear when only server is targeted")
	}
	if !strings.Contains(out, `"server/server.go"`) {
		t.Error("--target-files: server/server.go should appear in output")
	}
}

// ── --analysis-level 3 and --graph-field-depth ───────────────────────────────

func TestRootCmd_Level3Accepted(t *testing.T) {
	td := cliTestdataDir()

	out, _, err := runCmd(
		"--input", filepath.Join(td, "greeter"),
		"--analysis-level", "3",
		"--cache-dir", t.TempDir(),
	)
	if err != nil {
		t.Fatalf("-a 3 should succeed; got %v", err)
	}
	var v struct {
		MaxLevel int `json:"max_level"`
		KLimit   int `json:"k_limit"`
	}
	if jsonErr := json.Unmarshal([]byte(out), &v); jsonErr != nil {
		t.Fatalf("stdout is not valid JSON: %v", jsonErr)
	}
	if v.MaxLevel != 3 {
		t.Errorf("max_level = %d, want 3", v.MaxLevel)
	}
	if v.KLimit != 3 {
		t.Errorf("k_limit = %d, want 3 (the --graph-field-depth default)", v.KLimit)
	}
}

func TestRootCmd_UnsupportedLevelRejected(t *testing.T) {
	td := cliTestdataDir()

	for _, lvl := range []string{"0", "5", "-1"} {
		_, _, err := runCmd(
			"--input", filepath.Join(td, "greeter"),
			"--analysis-level", lvl,
			"--cache-dir", t.TempDir(),
		)
		if err == nil {
			t.Errorf("-a %s: expected error, got nil", lvl)
			continue
		}
		if !strings.Contains(err.Error(), "analysis level "+lvl+" is not supported") {
			t.Errorf("-a %s: error should name the unsupported level; got %q", lvl, err.Error())
		}
	}
}

func TestRootCmd_FieldDepthBelowLevel3Rejected(t *testing.T) {
	td := cliTestdataDir()

	_, _, err := runCmd(
		"--input", filepath.Join(td, "greeter"),
		"--analysis-level", "2",
		"--graph-field-depth", "2",
		"--cache-dir", t.TempDir(),
	)
	if err == nil {
		t.Fatal("expected error for --graph-field-depth at -a 2, got nil")
	}
	if !strings.Contains(err.Error(), "--graph-field-depth applies only at --analysis-level 3 or higher") {
		t.Errorf("error should explain the level gate; got %q", err.Error())
	}
}

func TestRootCmd_FieldDepthBelowOneRejected(t *testing.T) {
	td := cliTestdataDir()

	_, _, err := runCmd(
		"--input", filepath.Join(td, "greeter"),
		"--analysis-level", "3",
		"--graph-field-depth", "0",
		"--cache-dir", t.TempDir(),
	)
	if err == nil {
		t.Fatal("expected error for --graph-field-depth 0, got nil")
	}
	if !strings.Contains(err.Error(), "--graph-field-depth must be >= 1") {
		t.Errorf("error should explain the minimum; got %q", err.Error())
	}
}

func TestRootCmd_FieldDepthWithNeo4jRejected(t *testing.T) {
	td := cliTestdataDir()

	_, _, err := runCmd(
		"--input", filepath.Join(td, "greeter"),
		"--emit", "neo4j",
		"--graph-field-depth", "2",
	)
	if err == nil {
		t.Fatal("expected a flag error for --emit neo4j --graph-field-depth 2, got nil")
	}
	if !strings.Contains(err.Error(), "does not apply to --emit neo4j") {
		t.Errorf("error should explain the neo4j flag gate; got %q", err.Error())
	}
}

func TestRootCmd_EmitNeo4jRunsAtLevel3(t *testing.T) {
	td := cliTestdataDir()
	out := t.TempDir()
	if _, _, err := runCmd("--input", filepath.Join(td, "multipackage"), "--emit", "neo4j", "--output", out); err != nil {
		t.Fatalf("--emit neo4j should succeed; got %v", err)
	}
	data, err := os.ReadFile(filepath.Join(out, "graph.cypher"))
	if err != nil {
		t.Fatalf("graph.cypher not written: %v", err)
	}
	for _, rel := range []string{"GO_CFG_NEXT", "GO_CDG", "GO_DDG"} {
		if !strings.Contains(string(data), rel) {
			t.Errorf("graph.cypher should carry the L3 %s relationship", rel)
		}
	}
}

// ── malformed source ─────────────────────────────────────────────────────────

// TestRootCmd_MalformedSourceDoesNotPanic: testdata/malformed holds syntax
// errors (`if :=`, `range {`, `case 1, :`, `case <-:`), one per file, after
// which the parser recovers with a function node whose End is one past the
// end of the file: it has no token.File (the last file) or lies in the next
// one. Every level must still emit valid JSON, with the well-formed function
// that precedes each error present.
func TestRootCmd_MalformedSourceDoesNotPanic(t *testing.T) {
	td := cliTestdataDir()
	for _, level := range []string{"1", "2", "3"} {
		t.Run("a"+level, func(t *testing.T) {
			var out string
			var err error
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("-a %s panicked: %v", level, r)
					}
				}()
				out, _, err = runCmd("--input", filepath.Join(td, "malformed"), "-a", level, "--cache-dir", t.TempDir())
			}()
			if err != nil {
				t.Fatalf("-a %s failed: %v", level, err)
			}
			var v struct {
				Application struct {
					SymbolTable map[string]struct {
						Functions map[string]struct {
							Signature string `json:"signature"`
						} `json:"functions"`
					} `json:"symbol_table"`
				} `json:"application"`
			}
			if jsonErr := json.Unmarshal([]byte(out), &v); jsonErr != nil {
				t.Fatalf("-a %s stdout is not valid JSON: %v", level, jsonErr)
			}
			for file, want := range map[string]string{
				"if.go": "GoodIf", "range.go": "GoodRange", "case.go": "GoodCase", "recv.go": "GoodRecv",
			} {
				fns := v.Application.SymbolTable[file].Functions
				if _, ok := fns["example.com/malformed."+want]; !ok {
					t.Errorf("-a %s: %s: well-formed %s not emitted; have %v", level, file, want, fns)
				}
			}
		})
	}
}

// ── L4 scaffolding ───────────────────────────────────────────────────────────

const (
	errL4WithTargetFiles = "--target-files is not supported at analysis level 4 (whole-program); run without -t, or use -a 3"
	warnNeo4jTargetFiles = "--target-files: projecting the graph at level 3 (level 4 is whole-program); L4 rows keep the last full run's facts until a full run"
)

func TestCLI_Level4Accepted(t *testing.T) {
	td := cliTestdataDir()
	out, _, err := runCmd(
		"--input", filepath.Join(td, "greeter"),
		"--analysis-level", "4",
		"--graph-field-depth", "2",
		"--cache-dir", t.TempDir(),
	)
	if err != nil {
		t.Fatalf("-a 4 should succeed; got %v", err)
	}
	var v struct {
		MaxLevel int `json:"max_level"`
		KLimit   int `json:"k_limit"`
	}
	if jsonErr := json.Unmarshal([]byte(out), &v); jsonErr != nil {
		t.Fatalf("stdout is not valid JSON: %v", jsonErr)
	}
	if v.MaxLevel != 4 {
		t.Errorf("max_level = %d, want 4", v.MaxLevel)
	}
	if v.KLimit != 2 {
		t.Errorf("k_limit = %d, want 2 (--graph-field-depth is valid at -a 4)", v.KLimit)
	}
}

func TestCLI_Level4WithTargetFilesRejected(t *testing.T) {
	td := cliTestdataDir()
	_, _, err := runCmd(
		"--input", filepath.Join(td, "multipackage"),
		"--analysis-level", "4",
		"--target-files", filepath.Join(td, "multipackage", "server", "server.go"),
		"--cache-dir", t.TempDir(),
	)
	if err == nil {
		t.Fatal("expected an error for -a 4 -t, got nil")
	}
	if err.Error() != errL4WithTargetFiles {
		t.Errorf("error = %q, want %q", err.Error(), errL4WithTargetFiles)
	}
}

func TestCLI_Level3WithTargetFilesStillWorks(t *testing.T) {
	td := cliTestdataDir()
	out, _, err := runCmd(
		"--input", filepath.Join(td, "multipackage"),
		"--analysis-level", "3",
		"--target-files", filepath.Join(td, "multipackage", "server", "server.go"),
		"--cache-dir", t.TempDir(),
	)
	if err != nil {
		t.Fatalf("-a 3 -t should succeed; got %v", err)
	}
	if !strings.Contains(out, `"server/server.go"`) {
		t.Error("-a 3 -t: server/server.go should appear in output")
	}
}

func TestCLI_Neo4jWithTargetFilesFallsBackToL3(t *testing.T) {
	td := cliTestdataDir()
	out := t.TempDir()
	var runErr error
	stderr := captureStderr(t, func() {
		_, _, runErr = runCmd(
			"--input", filepath.Join(td, "multipackage"),
			"--emit", "neo4j",
			"--target-files", filepath.Join(td, "multipackage", "server", "server.go"),
			"--output", out,
			"--cache-dir", t.TempDir(),
		)
	})
	if runErr != nil {
		t.Fatalf("--emit neo4j -t should succeed; got %v", runErr)
	}
	if n := strings.Count(stderr, warnNeo4jTargetFiles); n != 1 {
		t.Errorf("D9 warning appeared %d times, want exactly 1; stderr:\n%s", n, stderr)
	}
	data, err := os.ReadFile(filepath.Join(out, "graph.cypher"))
	if err != nil {
		t.Fatalf("graph.cypher not written: %v", err)
	}
	cy := string(data)
	// Effective level 3: the :GoApplication node carries max_level 3 and the
	// L3 overlay is present.
	if !strings.Contains(cy, "max_level: 3") && !strings.Contains(cy, `"max_level":3`) && !strings.Contains(cy, "max_level = 3") {
		t.Errorf("graph.cypher does not record max_level 3")
	}
	for _, rel := range []string{"GO_CFG_NEXT", "GO_DDG"} {
		if !strings.Contains(cy, rel) {
			t.Errorf("graph.cypher should carry the L3 %s relationship", rel)
		}
	}
	// No L4 rows (R-P8): no MERGE of a GO_PARAM_IN / GO_PARAM_OUT /
	// GO_SUMMARY relationship, no port GO_DDG row, no vertex body node. The
	// same fixture without -t does carry each of them, so the guard is real.
	for rel, n := range l4CypherRows(cy) {
		if n != 0 {
			t.Errorf("graph.cypher under -t carries %d %s rows", n, rel)
		}
	}
	full := t.TempDir()
	if _, _, err := runCmd(
		"--input", filepath.Join(td, "multipackage"),
		"--emit", "neo4j",
		"--output", full,
		"--cache-dir", t.TempDir(),
	); err != nil {
		t.Fatalf("--emit neo4j should succeed; got %v", err)
	}
	data, err = os.ReadFile(filepath.Join(full, "graph.cypher"))
	if err != nil {
		t.Fatal(err)
	}
	for rel, n := range l4CypherRows(string(data)) {
		if n == 0 {
			t.Errorf("graph.cypher without -t carries no %s rows", rel)
		}
	}
}

// l4CypherRows counts the L4 rows of a graph.cypher script: the rows of
// each UNWIND batch that MERGEs a GO_PARAM_IN / GO_PARAM_OUT / GO_SUMMARY
// relationship, the port GO_DDG rows (prov reaching-defs), and the vertex
// :GoBodyNode rows (kind formal_in, formal_out, actual_in, actual_out).
func l4CypherRows(cy string) map[string]int {
	n := map[string]int{"GO_PARAM_IN": 0, "GO_PARAM_OUT": 0, "GO_SUMMARY": 0, "GO_DDG reaching-defs": 0, "vertex": 0}
	for _, stmt := range strings.Split(cy, ";\n") {
		rows := strings.Count(stmt, "\n  {")
		for _, rel := range []string{"GO_PARAM_IN", "GO_PARAM_OUT", "GO_SUMMARY"} {
			if strings.Contains(stmt, "MERGE (a)-[r:"+rel+"]->(b)") {
				n[rel] += rows
			}
		}
		if strings.Contains(stmt, "-[r:GO_DDG {_k: row.k}]->") {
			n["GO_DDG reaching-defs"] += strings.Count(stmt, "|reaching-defs'")
		}
		if strings.Contains(stmt, "MERGE (n:GoBodyNode") {
			for _, k := range []string{"formal_in", "formal_out", "actual_in", "actual_out"} {
				n["vertex"] += strings.Count(stmt, "kind: '"+k+"'")
			}
		}
	}
	return n
}

func TestCLI_Neo4jWithoutTargetFilesWarnsNothing(t *testing.T) {
	td := cliTestdataDir()
	var runErr error
	stderr := captureStderr(t, func() {
		_, _, runErr = runCmd(
			"--input", filepath.Join(td, "greeter"),
			"--emit", "neo4j",
			"--output", t.TempDir(),
			"--cache-dir", t.TempDir(),
		)
	})
	if runErr != nil {
		t.Fatalf("--emit neo4j should succeed; got %v", runErr)
	}
	if strings.Contains(stderr, "projecting the graph at level 3") {
		t.Errorf("the D9 warning must appear only under -t; stderr:\n%s", stderr)
	}
}

func TestCLI_StrictFlag(t *testing.T) {
	td := cliTestdataDir()
	if rootCmd().Flags().Lookup("strict") == nil {
		t.Fatal("--strict flag is not declared")
	}
	out, _, err := runCmd(
		"--input", filepath.Join(td, "greeter"),
		"--analysis-level", "4",
		"--strict",
		"--cache-dir", t.TempDir(),
	)
	if err != nil {
		t.Fatalf("-a 4 --strict should succeed on a clean project; got %v", err)
	}
	if !json.Valid([]byte(out)) {
		t.Error("stdout is not valid JSON")
	}
}

// ── --library-models ─────────────────────────────────────────────────────────

const errLibraryModelsLevel = "--library-models applies only at analysis level 4"

func writeModelsFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCLI_LibraryModelsBelowLevel4Rejected(t *testing.T) {
	td := cliTestdataDir()
	models := writeModelsFile(t, `{"strconv/Atoi": []}`)
	for _, level := range []string{"1", "2", "3"} {
		_, _, err := runCmd(
			"--input", filepath.Join(td, "greeter"),
			"--analysis-level", level,
			"--library-models", models,
			"--cache-dir", t.TempDir(),
		)
		if err == nil || err.Error() != errLibraryModelsLevel {
			t.Errorf("-a %s --library-models: error = %v, want %q", level, err, errLibraryModelsLevel)
		}
	}
}

func TestCLI_LibraryModelsAtLevel4AndNeo4j(t *testing.T) {
	td := cliTestdataDir()
	models := writeModelsFile(t, `{"fmt/Sprintf": [["format", "<return>"]]}`)
	out, _, err := runCmd(
		"--input", filepath.Join(td, "greeter"),
		"--analysis-level", "4",
		"--library-models", models,
		"--cache-dir", t.TempDir(),
	)
	if err != nil {
		t.Fatalf("-a 4 --library-models should succeed; got %v", err)
	}
	if !json.Valid([]byte(out)) {
		t.Error("stdout is not valid JSON")
	}
	if _, _, err := runCmd(
		"--input", filepath.Join(td, "greeter"),
		"--emit", "neo4j",
		"--library-models", models,
		"--output", t.TempDir(),
		"--cache-dir", t.TempDir(),
	); err != nil {
		t.Fatalf("--emit neo4j --library-models should succeed; got %v", err)
	}
}

// TestCLI_Neo4jTargetFilesWithLibraryModelsWarnsOnce: --emit neo4j -t runs at
// L3, so --library-models is unused there; the D9 warning says so on its one
// stderr line.
func TestCLI_Neo4jTargetFilesWithLibraryModelsWarnsOnce(t *testing.T) {
	td := cliTestdataDir()
	models := writeModelsFile(t, `{"fmt/Sprintf": []}`)
	var runErr error
	stderr := captureStderr(t, func() {
		_, _, runErr = runCmd(
			"--input", filepath.Join(td, "multipackage"),
			"--emit", "neo4j",
			"--target-files", filepath.Join(td, "multipackage", "server", "server.go"),
			"--library-models", models,
			"--output", t.TempDir(),
			"--cache-dir", t.TempDir(),
		)
	})
	if runErr != nil {
		t.Fatalf("--emit neo4j -t --library-models should succeed; got %v", runErr)
	}
	want := "[codeanalyzer-go WARN] " + warnNeo4jTargetFiles + "; --library-models is ignored at level 3\n"
	if n := strings.Count(stderr, want); n != 1 {
		t.Errorf("want exactly one line %q; stderr:\n%s", want, stderr)
	}
	if n := strings.Count(stderr, "projecting the graph at level 3"); n != 1 {
		t.Errorf("D9 warning appeared %d times, want 1; stderr:\n%s", n, stderr)
	}
}

func TestCLI_LibraryModelsBadFileErrors(t *testing.T) {
	td := cliTestdataDir()
	for name, path := range map[string]string{
		"missing":   filepath.Join(t.TempDir(), "absent.json"),
		"malformed": writeModelsFile(t, `{"fmt/Sprintf": [`),
	} {
		_, _, err := runCmd(
			"--input", filepath.Join(td, "greeter"),
			"--analysis-level", "4",
			"--library-models", path,
			"--cache-dir", t.TempDir(),
		)
		if err == nil || !strings.Contains(err.Error(), path) {
			t.Errorf("%s: error = %v, want one naming %s", name, err, path)
		}
	}
}

func TestNamespaceExplicitEmptyRejected(t *testing.T) {
	for _, flag := range []string{"--namespace", "--app-name"} {
		_, _, err := runCmd("--input", filepath.Join(cliTestdataDir(), "greeter"), flag, "", "--cache-dir", t.TempDir())
		if err == nil {
			t.Fatalf("accepted empty %s", flag)
		}
	}
}
func TestNamespaceExplicitScope(t *testing.T) {
	out, _, err := runCmd("--input", filepath.Join(cliTestdataDir(), "greeter"), "--namespace", "team space", "--app-name", "billing", "--cache-dir", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var a struct {
		IdentityVersion int `json:"identity_version"`
		Application     struct {
			ID        string `json:"id"`
			Namespace string `json:"identity_namespace"`
			Name      string `json:"application_name"`
		}
	}
	if err = json.Unmarshal([]byte(out), &a); err != nil {
		t.Fatal(err)
	}
	if a.IdentityVersion != 2 || a.Application.ID != "can://team%20space/billing" || a.Application.Namespace != "team space" || a.Application.Name != "billing" {
		t.Fatalf("%+v", a)
	}
}

func TestNamespaceDefaultUsesNormalizedInput(t *testing.T) {
	t.Chdir(filepath.Join(cliTestdataDir(), "greeter"))
	out, _, err := runCmd("--input", ".", "--cache-dir", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var a struct {
		Application struct {
			ID string `json:"id"`
		}
	}
	if err = json.Unmarshal([]byte(out), &a); err != nil {
		t.Fatal(err)
	}
	if a.Application.ID != "can://greeter/greeter" {
		t.Fatal(a.Application.ID)
	}
}
