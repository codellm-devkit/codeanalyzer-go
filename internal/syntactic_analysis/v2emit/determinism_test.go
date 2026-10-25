package v2emit

// Determinism gate for the v2 emitter.
//
// release-gates.md requires v2 output to be byte-identical across runs and
// across -j values. The symbol-table tree is already order-stable (modules
// iterated via sortedFileKeys), but the call_graph edge list is assembled by
// ranging a Go map in CallGraphBuilder.Build — random iteration order — and
// emitCallGraph preserved that order. Two runs of the same input therefore
// produced call_graph arrays in different orders.
//
// This gate emits the SAME resolved model twice and asserts the serialized
// call_graph is identical both times, so a regression to unsorted edge output
// fails here rather than only in an external CLI diff.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/codellm-devkit/codeanalyzer-go/internal/dataflow"
	"github.com/codellm-devkit/codeanalyzer-go/internal/schema"
	v2 "github.com/codellm-devkit/codeanalyzer-go/internal/schema/v2"
	"github.com/codellm-devkit/codeanalyzer-go/internal/semantic_analysis"
	"github.com/codellm-devkit/codeanalyzer-go/internal/syntactic_analysis"
)

// TestDeterminism_CallGraphStableAcrossEmits locks that emitting the same model
// twice yields a byte-identical call_graph. emitCallGraph must impose a total
// order on the edges rather than inherit map-iteration order.
func TestDeterminism_CallGraphStableAcrossEmits(t *testing.T) {
	dir := multipackageDir(t)
	b := syntactic_analysis.NewSymbolTableBuilder(dir)
	st, err := b.Build(nil, true)
	if err != nil {
		t.Fatalf("symbol table build failed: %v", err)
	}
	cg := semantic_analysis.NewCallGraphBuilder(dir, b.Fset(), b.Pkgs())
	edges := cg.Build(st)
	app := &schema.GoApplication{SymbolTable: st, CallGraph: edges}

	// Emit twice from the SAME model. If the edge order is derived from the
	// edges slice without a stable sort, repeated marshals still match (same
	// slice), so to actually exercise order-independence we marshal a shuffled
	// copy of the edge slice too.
	first := mustMarshalCallGraph(t, Emit(app, "multipackage", dir, 2, "det-test", 0, nil))

	// Reverse the input edge order and re-emit: a correct emitter sorts, so the
	// serialized call_graph must be unchanged.
	rev := make([]schema.GoCallEdge, len(edges))
	for i, e := range edges {
		rev[len(edges)-1-i] = e
	}
	appRev := &schema.GoApplication{SymbolTable: st, CallGraph: rev}
	second := mustMarshalCallGraph(t, Emit(appRev, "multipackage", dir, 2, "det-test", 0, nil))

	if first != second {
		t.Errorf("call_graph not order-independent: emitCallGraph must sort edges to a total order\nfirst=%s\nsecond=%s", first, second)
	}
}

func mustMarshalCallGraph(t *testing.T, out *v2.Analysis) string {
	t.Helper()
	type cgOnly struct {
		Application struct {
			CallGraph json.RawMessage `json:"call_graph"`
		} `json:"application"`
	}
	data, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	var c cgOnly
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	return string(c.Application.CallGraph)
}

// TestEncode_ByteIdenticalToMarshal locks the streaming writers to
// json.Marshal byte for byte (#27): sorted map keys, omitempty / omitzero,
// the call-node-only callee, HTML escaping (module sources carry `<-` and
// `&`), and no trailing newline. Encode streams a built tree; EmitTo builds
// and streams one module at a time. Both must equal json.Marshal(Emit(...)).
func TestEncode_ByteIdenticalToMarshal(t *testing.T) {
	for _, name := range []string{"chi", "dataflow", "generics"} {
		_, thisFile, _, _ := runtime.Caller(0)
		dir, err := filepath.Abs(filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		b := syntactic_analysis.NewSymbolTableBuilder(dir)
		st, err := b.Build(nil, true)
		if err != nil {
			t.Fatalf("%s: symbol table build failed: %v", name, err)
		}
		apps := map[int]*schema.GoApplication{1: {SymbolTable: st, CallGraph: []schema.GoCallEdge{}}}
		check := func(level int, app *schema.GoApplication, l3 map[dataflow.L3Key]dataflow.Result) {
			t.Run(fmt.Sprintf("%s/a%d", name, level), func(t *testing.T) {
				tree := Emit(app, name, dir, level, "enc-test", 3, l3)
				want, err := json.Marshal(tree)
				if err != nil {
					t.Fatal(err)
				}
				var enc bytes.Buffer
				if err := Encode(&enc, tree); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(enc.Bytes(), want) {
					t.Errorf("Encode differs from json.Marshal at byte %d (len %d vs %d)", firstDiff(enc.Bytes(), want), enc.Len(), len(want))
				}
				var lazy bytes.Buffer
				if err := EmitTo(&lazy, app, name, dir, level, "enc-test", 3, l3); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(lazy.Bytes(), want) {
					t.Errorf("EmitTo differs from json.Marshal(Emit) at byte %d (len %d vs %d)", firstDiff(lazy.Bytes(), want), lazy.Len(), len(want))
				}
			})
		}
		check(1, apps[1], nil)
		cg := semantic_analysis.NewCallGraphBuilder(dir, b.Fset(), b.Pkgs())
		edges := cg.Build(st)
		app := &schema.GoApplication{SymbolTable: st, CallGraph: edges, Externals: cg.Externals()}
		check(2, app, nil)
		check(3, app, dataflow.Run(b.Fset(), b.L3Units(st), 3, 4))
	}
}

func firstDiff(a, b []byte) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return min(len(a), len(b))
}
