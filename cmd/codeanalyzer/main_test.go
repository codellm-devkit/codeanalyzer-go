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
