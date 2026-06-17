package utils_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/codellm-devkit/codeanalyzer-go/internal/utils"
)

// ── IsTestFile ────────────────────────────────────────────────────────────────

func TestIsTestFile(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"foo_test.go", true},
		{"server_test.go", true},
		{"foo.go", false},
		{"test.go", false},   // doesn't end with _test.go
		{"_test.go", true},   // edge case: file is literally "_test.go"
		{"", false},
		{"foo_test.go.bak", false},
	}
	for _, tc := range tests {
		if got := utils.IsTestFile(tc.path); got != tc.want {
			t.Errorf("IsTestFile(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// ── IsVendored ────────────────────────────────────────────────────────────────

func TestIsVendored(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"vendor/github.com/foo/bar/baz.go", true},
		{"pkg/vendor/something.go", true},
		{"testdata/greeter/main.go", true},
		{".git/config", true},
		{"internal/core/analyzer.go", false},
		{"main.go", false},
		{"", false},
		{"vendored/not-vendor.go", false}, // "vendored" ≠ "vendor"
	}
	for _, tc := range tests {
		if got := utils.IsVendored(tc.path); got != tc.want {
			t.Errorf("IsVendored(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// ── FileHash ──────────────────────────────────────────────────────────────────

func TestFileHash_Deterministic(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(f, []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}

	h1, err := utils.FileHash(f)
	if err != nil {
		t.Fatalf("FileHash: %v", err)
	}
	h2, err := utils.FileHash(f)
	if err != nil {
		t.Fatalf("FileHash second call: %v", err)
	}
	if h1 != h2 {
		t.Errorf("FileHash is not deterministic: %q != %q", h1, h2)
	}
	if len(h1) != 64 {
		t.Errorf("FileHash should return 64-char hex SHA-256; got len %d: %q", len(h1), h1)
	}
}

func TestFileHash_DifferentContent(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.txt")
	b := filepath.Join(dir, "b.txt")
	os.WriteFile(a, []byte("aaa"), 0o644)
	os.WriteFile(b, []byte("bbb"), 0o644)

	ha, _ := utils.FileHash(a)
	hb, _ := utils.FileHash(b)
	if ha == hb {
		t.Error("different files should have different hashes")
	}
}

func TestFileHash_NonExistentFile(t *testing.T) {
	_, err := utils.FileHash(filepath.Join(t.TempDir(), "no-such-file"))
	if err == nil {
		t.Error("expected error for non-existent file, got nil")
	}
}

// ── EnsureDir ─────────────────────────────────────────────────────────────────

func TestEnsureDir_CreatesDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "b", "c")
	if err := utils.EnsureDir(dir); err != nil {
		t.Fatalf("EnsureDir: %v", err)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Errorf("directory %s was not created", dir)
	}
}

func TestEnsureDir_Idempotent(t *testing.T) {
	dir := t.TempDir()
	if err := utils.EnsureDir(dir); err != nil {
		t.Errorf("EnsureDir on existing dir: %v", err)
	}
}

// ── DiscoverGoFiles ───────────────────────────────────────────────────────────

func TestDiscoverGoFiles_FindsGoFiles(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "main.go", "package main")
	writeFile(t, dir, "util.go", "package main")

	files, err := utils.DiscoverGoFiles(dir, true)
	if err != nil {
		t.Fatalf("DiscoverGoFiles: %v", err)
	}
	if len(files) != 2 {
		t.Errorf("got %d files, want 2: %v", len(files), files)
	}
}

func TestDiscoverGoFiles_SkipsTestFiles(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "main.go", "package main")
	writeFile(t, dir, "main_test.go", "package main")

	files, err := utils.DiscoverGoFiles(dir, true)
	if err != nil {
		t.Fatalf("DiscoverGoFiles: %v", err)
	}
	for _, f := range files {
		if utils.IsTestFile(f) {
			t.Errorf("skipTests=true: found test file: %s", f)
		}
	}
}

func TestDiscoverGoFiles_IncludesTestFilesWhenNotSkipped(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "main.go", "package main")
	writeFile(t, dir, "main_test.go", "package main")

	files, err := utils.DiscoverGoFiles(dir, false)
	if err != nil {
		t.Fatalf("DiscoverGoFiles: %v", err)
	}
	if len(files) != 2 {
		t.Errorf("got %d files, want 2: %v", len(files), files)
	}
}

func TestDiscoverGoFiles_SkipsVendorDir(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "main.go", "package main")
	vendor := filepath.Join(dir, "vendor", "pkg")
	os.MkdirAll(vendor, 0o755)
	writeFile(t, vendor, "lib.go", "package pkg")

	files, err := utils.DiscoverGoFiles(dir, true)
	if err != nil {
		t.Fatalf("DiscoverGoFiles: %v", err)
	}
	if len(files) != 1 {
		t.Errorf("got %d files, want 1 (vendor should be skipped): %v", len(files), files)
	}
}

func TestDiscoverGoFiles_IgnoresNonGoFiles(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "main.go", "package main")
	writeFile(t, dir, "readme.md", "# readme")
	writeFile(t, dir, "config.yaml", "key: val")

	files, err := utils.DiscoverGoFiles(dir, true)
	if err != nil {
		t.Fatalf("DiscoverGoFiles: %v", err)
	}
	if len(files) != 1 {
		t.Errorf("got %d files, want 1: %v", len(files), files)
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writeFile %s: %v", name, err)
	}
}
