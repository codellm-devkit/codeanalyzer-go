package repository

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// skipIfCannotDenyRead skips where a permission-denied read cannot be produced:
// Windows has no mode bits, and root reads everything regardless of mode.
func skipIfCannotDenyRead(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("mode bits do not deny reads on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses mode bits")
	}
}

// Item 3: one unreadable directory must not abort discovery. Before the fix,
// Discover returned "artifact discovery: open .../locked: permission denied" and
// the whole analysis failed, though only that directory's contents were lost.
func TestDiscover_UnreadableDirectoryIsSkippedNotFatal(t *testing.T) {
	skipIfCannotDenyRead(t)
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"go.mod":       "module example.com/x\n",
		"README.md":    "# readme\n",
		"locked/a.txt": "secret\n",
	})
	locked := filepath.Join(root, "locked")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	got, err := Discover(root, Options{})
	if err != nil {
		t.Fatalf("one unreadable directory aborted discovery: %v", err)
	}
	if _, ok := got["README.md"]; !ok {
		t.Errorf("readable sibling README.md was lost: %v", keys(got))
	}
	if _, ok := got["locked/a.txt"]; ok {
		t.Errorf("file inside an unreadable directory was captured")
	}
}

// Item 3, file level: an unreadable file is skipped; the rest is still discovered.
func TestDiscover_UnreadableFileIsSkippedNotFatal(t *testing.T) {
	skipIfCannotDenyRead(t)
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"go.mod":    "module example.com/x\n",
		"README.md": "# readme\n",
		"noread.md": "hidden\n",
	})
	noread := filepath.Join(root, "noread.md")
	if err := os.Chmod(noread, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(noread, 0o644) })

	got, err := Discover(root, Options{})
	if err != nil {
		t.Fatalf("one unreadable file aborted discovery: %v", err)
	}
	if _, ok := got["README.md"]; !ok {
		t.Errorf("readable sibling README.md was lost: %v", keys(got))
	}
}

// The root itself stays fatal: without it there is nothing to analyze, and an
// empty artifact list would look like a clean run.
func TestDiscover_MissingRootIsFatal(t *testing.T) {
	if _, err := Discover(filepath.Join(t.TempDir(), "absent"), Options{}); err == nil {
		t.Fatal("a missing project root must be an error, not an empty artifact list")
	}
}

// Item 5: hashing must not load the file. A 64 MiB sparse file is hashed while the
// heap allocates far less than its size; os.ReadFile would allocate all 64 MiB.
func TestDiscover_LargeBinaryIsNotLoadedWhole(t *testing.T) {
	const size = 64 << 20
	root := t.TempDir()
	writeTree(t, root, map[string]string{"go.mod": "module example.com/x\n"})
	f, err := os.Create(filepath.Join(root, "model.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	got, err := Discover(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	if a := got["model.bin"]; a.SizeBytes != size {
		t.Fatalf("size = %d, want %d", a.SizeBytes, size)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 8<<20 {
		t.Errorf("discovery allocated %d MiB for a %d MiB file: the file is read whole",
			allocated>>20, size>>20)
	}
}

// A binary named like a Dockerfile must still be binary. Before the fix the name
// check ran first, so the file was classified "dockerfile" and, with
// --artifact-text, its raw bytes were captured as text.
func TestDiscover_BinaryNamedLikeDockerfileStaysBinary(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"go.mod": "module example.com/x\n"})
	if err := os.WriteFile(filepath.Join(root, "Dockerfile.bin"), []byte("\x7fELF\x00\x01\x02"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Discover(root, Options{CaptureText: true})
	if err != nil {
		t.Fatal(err)
	}
	a := got["Dockerfile.bin"]
	if a.Format != "binary" {
		t.Errorf("format = %q, want binary", a.Format)
	}
	if a.Source != "" {
		t.Errorf("binary file's bytes were captured as text: %q", a.Source)
	}
}

// Security: a path the walk saw as a regular file must still be that file when it
// is opened. A file swapped for a symlink (to, say, a secret outside the project)
// after the walk must be refused, not followed.
func TestReadArtifact_RefusesPathSwappedForSymlinkAfterWalk(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on Windows")
	}
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("TOP SECRET\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(target, []byte("ordinary\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	walked, err := os.Lstat(target) // what the walk recorded
	if err != nil {
		t.Fatal(err)
	}
	// The swap happens between the walk and the open.
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, target); err != nil {
		t.Fatal(err)
	}
	art, err := readArtifact(target, walked, "notes.txt", true)
	if err == nil {
		t.Fatalf("symlink swapped in after the walk was read: %+v", art)
	}
	if art.Source != "" {
		t.Errorf("secret captured through the swapped symlink: %q", art.Source)
	}
}

// A path that now names a different regular file than the walk recorded is refused.
func TestReadArtifact_RefusesDifferentFileAtSamePath(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.txt")
	b := filepath.Join(dir, "b.txt")
	for _, p := range []string{a, b} {
		if err := os.WriteFile(p, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	infoB, err := os.Lstat(b)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readArtifact(a, infoB, "a.txt", false); err == nil {
		t.Fatal("a file other than the walked one was read")
	}
}
