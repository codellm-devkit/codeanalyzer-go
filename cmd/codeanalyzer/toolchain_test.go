package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// captureStderr runs fn with os.Stderr redirected (utils.Warn writes there,
// not to the cobra command's stderr) and returns what was written.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	defer func() { os.Stderr = orig }()
	fn()
	w.Close()
	os.Stderr = orig
	return <-done
}

// TestRootCmd_NewerGoVersionWarnsOnce (I4): packages that need a newer Go than
// the analyzer was built with type-check partially and the run still exits 0,
// so the degradation must be loud. Two such packages (go1.99, go1.98) give ONE
// consolidated warning naming the build Go, the highest version required and
// the affected-package count; stdout carries the analysis, untouched.
//
// The fixture's go.mod cannot itself say go 1.99: the go command refuses such
// a module before loading anything (no toolchain to switch to), so the run
// fails outright. Files gated by //go:build go1.99 / go1.98, with the tags set
// through GOFLAGS, reach go/types' same "requires newer Go version" check.
func TestRootCmd_NewerGoVersionWarnsOnce(t *testing.T) {
	t.Setenv("GOFLAGS", "-tags=go1.99,go1.98")
	t.Setenv("GOTOOLCHAIN", "local")
	var out string
	var runErr error
	stderr := captureStderr(t, func() {
		out, _, runErr = runCmd("--input", filepath.Join(cliTestdataDir(), "futurego"), "--cache-dir", t.TempDir())
	})
	if runErr != nil {
		t.Fatalf("command failed: %v\nstderr:\n%s", runErr, stderr)
	}

	var warns []string
	for _, line := range strings.Split(stderr, "\n") {
		if strings.Contains(line, "WARN") && strings.Contains(strings.ToLower(line), "rebuild") {
			warns = append(warns, line)
		}
	}
	if len(warns) != 1 {
		t.Fatalf("want exactly one consolidated warning, got %d:\n%s", len(warns), stderr)
	}
	w := warns[0]
	for _, want := range []string{runtime.Version(), "go1.99", "2 packages"} {
		if !strings.Contains(w, want) {
			t.Errorf("warning lacks %q: %s", want, w)
		}
	}
	if strings.Contains(w, "go1.98") {
		t.Errorf("warning names a lower version than the highest: %s", w)
	}

	// (stdout embeds module sources, whose comments mention "newer Go
	// version"; look for the log line's prefix instead.)
	if strings.Contains(out, "[codeanalyzer-go") {
		t.Errorf("a log line leaked onto stdout")
	}
	var v struct {
		Application struct {
			SymbolTable map[string]json.RawMessage `json:"symbol_table"`
		} `json:"application"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("stdout is not valid JSON: %v", err)
	}
	for _, rel := range []string{"a/a.go", "b/b.go", "c/c.go"} {
		if _, ok := v.Application.SymbolTable[rel]; !ok {
			t.Errorf("stdout symbol_table lacks %s", rel)
		}
	}
}
