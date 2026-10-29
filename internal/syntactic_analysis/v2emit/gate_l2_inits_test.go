package v2emit

// Repeated `<pkg>.init` gate. Go allows a func init() in every file of a
// package, and all of them share the signature "<pkg>.init". The resolver must
// keep each init's own call edges: when two inits call the same callee, both
// edges survive, and the result does not depend on map iteration order.
// Fixture: testdata/dataflow/inits (a.go and b.go each declare init(), both
// call the in-package setup() and the library os.Getenv).

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/codellm-devkit/codeanalyzer-go/internal/schema"
	v2 "github.com/codellm-devkit/codeanalyzer-go/internal/schema/v2"
	"github.com/codellm-devkit/codeanalyzer-go/internal/semantic_analysis"
	"github.com/codellm-devkit/codeanalyzer-go/internal/syntactic_analysis"
)

// buildDataflowL2Fresh runs an independent L2 analysis of testdata/dataflow
// (new symbol table, resolver and externals), so map order varies per call.
func buildDataflowL2Fresh(t *testing.T) *v2.Analysis {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	dir, err := filepath.Abs(filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "testdata", "dataflow"))
	if err != nil {
		t.Fatalf("resolving fixture dir: %v", err)
	}
	b := syntactic_analysis.NewSymbolTableBuilder(dir)
	st, err := b.Build(nil, true)
	if err != nil {
		t.Fatalf("symbol table build failed: %v", err)
	}
	cg := semantic_analysis.NewCallGraphBuilder(dir, b.Fset(), b.Pkgs())
	edges := cg.Build(st)
	app := &schema.GoApplication{SymbolTable: st, CallGraph: edges, Externals: cg.Externals()}
	return Emit(app, "dataflow", dir, 2, "gate-test", 0, nil, nil)
}

// TestGateL2_EveryInitKeepsItsEdges: each file's init has its own edge to the
// shared in-package callee and to the shared library callee.
func TestGateL2_EveryInitKeepsItsEdges(t *testing.T) {
	out := buildDataflowL2Fresh(t)
	const initSig = "example.com/dataflow/inits.init"
	setup := functionByName(t, mustModule(t, out, "inits/a.go"), "setup").ID
	getenv := v2.ExternalID(out.Application.ID, "os", "Getenv")

	has := map[[2]string]bool{}
	for _, e := range out.Application.CallGraph {
		has[[2]string{e.Src, e.Dst}] = true
	}
	for _, rel := range []string{"inits/a.go", "inits/b.go"} {
		fn, ok := mustModule(t, out, rel).Functions[initSig]
		if !ok {
			t.Fatalf("%s: init missing", rel)
		}
		for _, dst := range []string{setup, getenv} {
			if !has[[2]string{fn.ID, dst}] {
				t.Errorf("missing call_graph edge %s -> %s", fn.ID, dst)
			}
		}
	}
}

// TestGateL2_InitsFreshBuildsByteIdentical: 10 fresh builds of the dataflow
// fixture serialize to byte-identical JSON.
func TestGateL2_InitsFreshBuildsByteIdentical(t *testing.T) {
	var want []byte
	for i := 0; i < 10; i++ {
		got, err := json.Marshal(buildDataflowL2Fresh(t))
		if err != nil {
			t.Fatalf("build %d: marshal: %v", i, err)
		}
		if i == 0 {
			want = got
			continue
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("build %d differs from build 0", i)
		}
	}
}
