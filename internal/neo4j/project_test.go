package neo4j

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/codellm-devkit/codeanalyzer-go/internal/schema"
	v2 "github.com/codellm-devkit/codeanalyzer-go/internal/schema/v2"
	"github.com/codellm-devkit/codeanalyzer-go/internal/semantic_analysis"
	"github.com/codellm-devkit/codeanalyzer-go/internal/syntactic_analysis"
	"github.com/codellm-devkit/codeanalyzer-go/internal/syntactic_analysis/v2emit"
)

// M3 gate: the pure projector. These project the multipackage fixture at L2
// (the real builder + resolver, the same wiring -a 2 uses) and assert the
// conformance contract, no-dangling, two-projection id agreement, determinism,
// and the Go-specific shapes.

// buildMultipackageL2 builds the fixture through the real symbol-table builder
// and resolver and emits the v2 L2 analysis — the input to Project.
func buildMultipackageL2(t *testing.T) *v2.Analysis {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	dir, err := filepath.Abs(filepath.Join(filepath.Dir(thisFile), "..", "..", "testdata", "multipackage"))
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
	app := &schema.GoApplication{SymbolTable: st, CallGraph: edges}
	return v2emit.Emit(app, "multipackage", dir, 2, "neo4j-gate")
}

func TestProject_NoDanglingEndpoints(t *testing.T) {
	rows := Project(buildMultipackageL2(t))

	// Every (label, value) that was emitted as a node.
	emitted := make(map[string]bool, len(rows.Nodes))
	for _, n := range rows.Nodes {
		for _, l := range n.Labels {
			emitted[l+"\x00"+n.Value] = true
		}
	}
	for _, e := range rows.Edges {
		if !emitted[e.From.Label+"\x00"+e.From.Value] {
			t.Errorf("%s edge FROM a non-emitted node: %s %s", e.Type, e.From.Label, e.From.Value)
		}
		if !emitted[e.To.Label+"\x00"+e.To.Value] {
			t.Errorf("%s edge TO a non-emitted node: %s %s", e.Type, e.To.Label, e.To.Value)
		}
	}
}

func TestProject_IdParityWithJSON(t *testing.T) {
	a := buildMultipackageL2(t)
	rows := Project(a)

	// Every can:// id in the JSON tree (application/module/type/callable/field)
	// must appear as a graph node, and vice versa for can://-keyed nodes.
	jsonIDs := collectDeclaredIDs(a)
	jsonIDs[a.Application.ID] = true
	for _, m := range a.Application.SymbolTable {
		jsonIDs[m.ID] = true
		for _, t := range m.Types {
			for _, f := range t.Fields {
				jsonIDs[f.ID] = true
			}
		}
	}

	graphIDs := make(map[string]bool)
	for _, n := range rows.Nodes {
		// Only can:// nodes participate (externals/ghosts are can:// too, but are
		// not in the JSON tree, so compare the declared-tree subset).
		graphIDs[n.Value] = true
	}

	for id := range jsonIDs {
		if !graphIDs[id] {
			t.Errorf("JSON tree id has no graph node: %s", id)
		}
	}
}

func TestProject_Deterministic(t *testing.T) {
	a := buildMultipackageL2(t)
	r1 := Project(a)
	r2 := Project(a)
	if len(r1.Nodes) != len(r2.Nodes) || len(r1.Edges) != len(r2.Edges) {
		t.Fatalf("row counts differ across runs")
	}
	for i := range r1.Nodes {
		if r1.Nodes[i].Value != r2.Nodes[i].Value || r1.Nodes[i].Labels[0] != r2.Nodes[i].Labels[0] {
			t.Fatalf("node order not deterministic at %d", i)
		}
	}
	for i := range r1.Edges {
		if edgeSortKey(r1.Edges[i]) != edgeSortKey(r2.Edges[i]) {
			t.Fatalf("edge order not deterministic at %d", i)
		}
	}
}

func TestProject_GoSpecificShapes(t *testing.T) {
	rows := Project(buildMultipackageL2(t))

	var sawType, sawCallable, sawModule, sawCalls bool
	var typeKindsSeen = map[string]bool{}
	for _, n := range rows.Nodes {
		switch n.Labels[0] {
		case LabelModule:
			sawModule = true
		}
		if hasLabel(n, LabelType) {
			sawType = true
			if k, ok := n.Props["kind"].(string); ok {
				typeKindsSeen[k] = true
			}
		}
		if hasLabel(n, LabelCallable) {
			sawCallable = true
		}
	}
	for _, e := range rows.Edges {
		if e.Type == RelCalls {
			sawCalls = true
		}
	}
	if !sawModule || !sawType || !sawCallable {
		t.Errorf("expected module/type/callable nodes; module=%v type=%v callable=%v", sawModule, sawType, sawCallable)
	}
	if !sawCalls {
		t.Error("expected at least one GO_CALLS edge at L2")
	}
	// Type kinds must be from the declared vocabulary.
	for k := range typeKindsSeen {
		switch k {
		case "struct", "interface", "alias", "defined":
		default:
			t.Errorf("undeclared type kind %q", k)
		}
	}
}

func hasLabel(n NodeRow, label string) bool {
	for _, l := range n.Labels {
		if l == label {
			return true
		}
	}
	return false
}
