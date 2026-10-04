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

func TestRootCmd_EmitNeo4jNotImplemented(t *testing.T) {
	td := cliTestdataDir()
	_, _, err := runCmd("--input", filepath.Join(td, "greeter"), "--emit", "neo4j")
	if err == nil {
		t.Fatal("expected non-zero exit for --emit neo4j, got nil")
	}
	if !strings.Contains(err.Error(), "not yet implemented") {
		t.Errorf("error should say 'not yet implemented'; got %q", err.Error())
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

	var result struct {
		CallGraph []interface{} `json:"call_graph"`
	}
	if jsonErr := json.Unmarshal([]byte(out), &result); jsonErr != nil {
		t.Fatalf("stdout is not valid JSON: %v", jsonErr)
	}
	if len(result.CallGraph) != 0 {
		t.Errorf("level 1 should produce no call graph edges; got %d", len(result.CallGraph))
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

	var result struct {
		CallGraph []interface{} `json:"call_graph"`
	}
	if jsonErr := json.Unmarshal([]byte(out), &result); jsonErr != nil {
		t.Fatalf("stdout is not valid JSON: %v", jsonErr)
	}
	if len(result.CallGraph) == 0 {
		t.Error("level 2 should produce call graph edges; got none")
	}
}

// ── --analysis-schema ──────────────────────────────────────────────────────────

func TestRootCmd_DefaultSchemaIsV1(t *testing.T) {
	td := cliTestdataDir()
	out, _, err := runCmd("--input", filepath.Join(td, "greeter"), "--cache-dir", t.TempDir())
	if err != nil {
		t.Fatalf("command failed: %v", err)
	}
	var v map[string]interface{}
	if jsonErr := json.Unmarshal([]byte(out), &v); jsonErr != nil {
		t.Fatalf("stdout is not valid JSON: %v", jsonErr)
	}
	if _, ok := v["symbol_table"]; !ok {
		t.Error("default output should be the v1 shape (top-level symbol_table)")
	}
	if _, ok := v["schema_version"]; ok {
		t.Error("default output should not carry schema_version (that is v2)")
	}
}

func TestRootCmd_AnalysisSchema2EmitsV2(t *testing.T) {
	td := cliTestdataDir()
	out, _, err := runCmd(
		"--input", filepath.Join(td, "greeter"),
		"--analysis-schema", "2",
		"--cache-dir", t.TempDir(),
	)
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
	if v.Application.ID != "can://go/greeter" {
		t.Errorf("application.id = %q, want can://go/greeter", v.Application.ID)
	}
}

func TestRootCmd_UnknownSchemaReturnsError(t *testing.T) {
	td := cliTestdataDir()
	_, _, err := runCmd(
		"--input", filepath.Join(td, "greeter"),
		"--analysis-schema", "9",
		"--cache-dir", t.TempDir(),
	)
	if err == nil {
		t.Fatal("expected error for unknown --analysis-schema value, got nil")
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
