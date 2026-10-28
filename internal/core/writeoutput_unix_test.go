//go:build unix

package core_test

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/codellm-devkit/codeanalyzer-go/internal/core"
	"github.com/codellm-devkit/codeanalyzer-go/internal/options"
)

// TestWriteOutput_ModeHonoursUmask: analysis.json gets the mode os.Create
// would give it (0666 less the umask), not CreateTemp's 0600, and the temp
// file is gone after the rename.
func TestWriteOutput_ModeHonoursUmask(t *testing.T) {
	for _, mask := range []int{0o022, 0o077} {
		old := syscall.Umask(mask)
		outDir := t.TempDir()
		err := core.WriteOutput(sharedGreeterL1, nil, nil, options.AnalysisOptions{OutputDir: outDir, Format: "json"})
		syscall.Umask(old)
		if err != nil {
			t.Fatalf("umask %#o: WriteOutput: %v", mask, err)
		}
		st, err := os.Stat(filepath.Join(outDir, "analysis.json"))
		if err != nil {
			t.Fatal(err)
		}
		if want := os.FileMode(0o666 &^ mask); st.Mode().Perm() != want {
			t.Errorf("umask %#o: mode = %v, want %v", mask, st.Mode().Perm(), want)
		}
		left, _ := filepath.Glob(filepath.Join(outDir, ".analysis.json.*"))
		if len(left) != 0 {
			t.Errorf("umask %#o: temp files left behind: %v", mask, left)
		}
	}
}
