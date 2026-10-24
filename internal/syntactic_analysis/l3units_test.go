package syntactic_analysis_test

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/codellm-devkit/codeanalyzer-go/internal/dataflow"
	"github.com/codellm-devkit/codeanalyzer-go/internal/syntactic_analysis"
)

// TestL3Units_UnmatchedClosureSkipped: a function literal whose L1 closure
// entry does not match (here its recorded start line is off by one) gets no
// unit — never another closure's key — while its enclosing callable keeps
// its unit.
func TestL3Units_UnmatchedClosureSkipped(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(thisFile), "..", "..", "testdata", "dataflow")
	b := syntactic_analysis.NewSymbolTableBuilder(dir)
	st, err := b.Build(nil, true)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("flow", "flow.go")
	outer := dataflow.L3Key{Path: path, Signature: "example.com/dataflow/flow.Closure"}
	closure := dataflow.L3Key{Path: path, Signature: outer.Signature + ".closure_1"}

	has := func() map[dataflow.L3Key]bool {
		m := map[dataflow.L3Key]bool{}
		for _, u := range b.L3Units(st) {
			m[u.Key] = true
		}
		return m
	}
	if got := has(); !got[outer] || !got[closure] {
		t.Fatalf("baseline: outer %v, closure %v; want both", got[outer], got[closure])
	}

	fn := st[path].Functions[outer.Signature]
	ic := fn.InnerCallables["closure_1"]
	ic.StartLine++
	fn.InnerCallables["closure_1"] = ic

	got := has()
	if got[closure] {
		t.Error("mismatched closure still got a unit")
	}
	if !got[outer] {
		t.Error("enclosing callable lost its unit")
	}
}
