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
	"encoding/json"
	"testing"

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
	first := mustMarshalCallGraph(t, Emit(app, "multipackage", dir, 2, "det-test"))

	// Reverse the input edge order and re-emit: a correct emitter sorts, so the
	// serialized call_graph must be unchanged.
	rev := make([]schema.GoCallEdge, len(edges))
	for i, e := range edges {
		rev[len(edges)-1-i] = e
	}
	appRev := &schema.GoApplication{SymbolTable: st, CallGraph: rev}
	second := mustMarshalCallGraph(t, Emit(appRev, "multipackage", dir, 2, "det-test"))

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
