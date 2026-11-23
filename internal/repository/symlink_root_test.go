package repository

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"testing"
)

// symlinkTree makes a project at <base>/real and a symlink <base>/linked to it.
func symlinkTree(t *testing.T) (real, linked string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("directory symlinks need privileges on Windows")
	}
	base := t.TempDir()
	real = filepath.Join(base, "real")
	writeTree(t, real, map[string]string{
		"go.mod":    "module example.com/app\n\ngo 1.22\n\nrequire example.com/dep v1.0.0\n",
		"go.sum":    "example.com/dep v1.0.0 h1:AAAA=\n",
		"README.md": "# readme\n",
		"main.go":   "package main\n",
	})
	linked = filepath.Join(base, "linked")
	if err := os.Symlink(real, linked); err != nil {
		t.Fatal(err)
	}
	return real, linked
}

// A project reached through a symlinked root must produce the same artifacts as
// the real path. Before the fix, WalkDir saw the root as one non-directory entry
// and returned no artifacts at all.
func TestDiscover_SymlinkedRootMatchesRealRoot(t *testing.T) {
	real, linked := symlinkTree(t)
	want, err := Discover(real, Options{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := Discover(linked, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(want) == 0 {
		t.Fatal("real root produced no artifacts; the fixture is wrong")
	}
	if !reflect.DeepEqual(sortedKeysOf(got), sortedKeysOf(want)) {
		t.Fatalf("symlinked root artifacts = %v, want %v", keys(got), keys(want))
	}
}

// An output directory given through a symlink must still be excluded, or the
// previous run's analysis.json is captured as an artifact.
func TestDiscover_SymlinkedOutputDirIsExcluded(t *testing.T) {
	real, _ := symlinkTree(t)
	outReal := filepath.Join(real, "out")
	if err := os.MkdirAll(outReal, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outReal, "analysis.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	outLink := filepath.Join(t.TempDir(), "out-link")
	if err := os.Symlink(outReal, outLink); err != nil {
		t.Fatal(err)
	}
	// Walk the real root, so analysis.json is reachable; only the output dir is a symlink.
	got, err := Discover(real, Options{OutputDir: outLink})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got["out/analysis.json"]; ok {
		t.Fatalf("previous analysis.json was captured through a symlinked output dir: %v", keys(got))
	}
}

func sortedKeysOf[V any](m map[string]V) []string {
	ks := keys(m)
	sort.Strings(ks)
	return ks
}

// resolveDir accepts a path that does not exist yet (an output directory created
// later), resolving its existing ancestor and keeping the missing tail.
func TestResolveDir_MissingTailIsKept(t *testing.T) {
	real, linked := symlinkTree(t)
	got, err := resolveDir(filepath.Join(linked, "not-yet", "deeper"))
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(real, "not-yet", "deeper")
	if evalReal, err := filepath.EvalSymlinks(real); err == nil {
		want = filepath.Join(evalReal, "not-yet", "deeper")
	}
	if got != want {
		t.Fatalf("resolveDir = %q, want %q", got, want)
	}
}
