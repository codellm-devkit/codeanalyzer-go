// Package utils provides filesystem helpers and logging utilities.
package utils

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// IsTestFile reports whether path is a Go test file (_test.go suffix).
func IsTestFile(path string) bool {
	return strings.HasSuffix(path, "_test.go")
}

// IsVendored reports whether path is under a vendored or generated directory.
func IsVendored(path string) bool {
	for _, seg := range strings.Split(filepath.ToSlash(path), "/") {
		switch seg {
		case "vendor", "testdata", ".git":
			return true
		}
	}
	return false
}

// RelativePath returns path relative to root, or path itself on error.
func RelativePath(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return rel
}

// FileHash returns the SHA-256 hex digest of the file at path.
func FileHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

// DiscoverGoFiles returns all *.go files under root, skipping vendored dirs
// and optionally test files.
func DiscoverGoFiles(root string, skipTests bool) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries gracefully
		}
		if d.IsDir() {
			if IsVendored(path) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		if skipTests && IsTestFile(path) {
			return nil
		}
		files = append(files, path)
		return nil
	})
	return files, err
}

// EnsureDir creates dir and all parents if they don't exist.
func EnsureDir(dir string) error {
	return os.MkdirAll(dir, 0o755)
}
