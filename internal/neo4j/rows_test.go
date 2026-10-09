package neo4j

import (
	"reflect"
	"testing"
)

// M2 gate: the RowBuilder's in-memory MERGE semantics, the edge-only-when-
// resolved rule, pruning, deterministic finish(), scoping-helper refusal, and
// Cypher-literal rendering.

func TestRowBuilder_NodeMergeDedupsAndUnionsLabels(t *testing.T) {
	b := NewRowBuilder()
	id := "can://go/app/x.go/T"
	b.Node([]string{SymbolLabel, LabelType}, "id", id, Props{"kind": "struct"}, "x.go")
	// Re-see the same identity with an extra label + an extra prop.
	b.Node([]string{SymbolLabel, MarkerLabel}, "id", id, Props{"signature": "T"}, "x.go")

	rows := b.Finish()
	if len(rows.Nodes) != 1 {
		t.Fatalf("re-seeing an identity must dedup to one node; got %d", len(rows.Nodes))
	}
	n := rows.Nodes[0]
	// Labels unioned, MERGE label first.
	want := []string{SymbolLabel, LabelType, MarkerLabel}
	if !reflect.DeepEqual(n.Labels, want) {
		t.Errorf("labels = %v, want %v", n.Labels, want)
	}
	// Props merged (last-write-wins union).
	if n.Props["kind"] != "struct" || n.Props["signature"] != "T" {
		t.Errorf("props not merged across re-see: %v", n.Props)
	}
}

func TestRowBuilder_PropLastWriteWins(t *testing.T) {
	b := NewRowBuilder()
	id := "can://go/app/x.go/T"
	b.Node([]string{LabelType}, "id", id, Props{"kind": "struct"}, "")
	b.Node([]string{LabelType}, "id", id, Props{"kind": "interface"}, "")
	rows := b.Finish()
	if rows.Nodes[0].Props["kind"] != "interface" {
		t.Errorf("last write must win: kind = %v, want interface", rows.Nodes[0].Props["kind"])
	}
}

func TestRowBuilder_DeferredEdgeDroppedWhenTargetAbsent(t *testing.T) {
	b := NewRowBuilder()
	src := b.Node([]string{SymbolLabel, LabelType}, "id", "can://go/app/x.go/T", nil, "x.go")
	// Target never emitted as a node.
	b.EdgeToSymbol(RelEmbeds, src, SymbolLabel, "id", "can://go/app/y.go/Missing", nil, "")
	rows := b.Finish()
	if len(rows.Edges) != 0 {
		t.Errorf("deferred edge to an unemitted target must be dropped; got %d edges", len(rows.Edges))
	}
}

func TestRowBuilder_DeferredEdgeKeptWhenTargetEmitted(t *testing.T) {
	b := NewRowBuilder()
	src := b.Node([]string{SymbolLabel, LabelType}, "id", "can://go/app/x.go/T", nil, "x.go")
	target := "can://go/app/y.go/Base"
	b.Node([]string{SymbolLabel, LabelType}, "id", target, nil, "y.go")
	b.EdgeToSymbol(RelEmbeds, src, SymbolLabel, "id", target, nil, "")
	rows := b.Finish()
	if len(rows.Edges) != 1 {
		t.Fatalf("deferred edge to an emitted target must be kept; got %d edges", len(rows.Edges))
	}
	if rows.Edges[0].Type != RelEmbeds || rows.Edges[0].To.Value != target {
		t.Errorf("resolved edge wrong: %+v", rows.Edges[0])
	}
}

func TestPrune_DropsNilAndEmptySlice(t *testing.T) {
	got := prune(Props{
		"a":     "keep",
		"b":     nil,
		"empty": []string{},
		"list":  []string{"x"},
		"zero":  0, // 0 is a real value, not absence — kept.
		"false": false,
	})
	if _, ok := got["b"]; ok {
		t.Error("nil prop must be pruned")
	}
	if _, ok := got["empty"]; ok {
		t.Error("empty slice must be pruned")
	}
	if got["a"] != "keep" || got["zero"] != 0 || got["false"] != false || !reflect.DeepEqual(got["list"], []string{"x"}) {
		t.Errorf("prune dropped a real value: %v", got)
	}
}

func TestPrune_AllEmptyReturnsNil(t *testing.T) {
	if prune(Props{"a": nil}) != nil {
		t.Error("an all-empty bag must prune to nil")
	}
	if prune(nil) != nil {
		t.Error("nil bag must prune to nil")
	}
}

func TestApplicationPrefix_RefusesNonBareID(t *testing.T) {
	cases := map[string]bool{
		"can://go/app":        true,  // bare — accepted
		"can://go/my-app":     true,  //
		"":                    false, // empty
		"can://go":            false, // missing app segment
		"can://go/app/x.go":   false, // too deep (a module id)
		"can://go/app/x.go/T": false, // deeper still
		"go/app":              false, // no scheme
		"can://go//":          false, // empty segment
	}
	for id, wantOK := range cases {
		_, ok := applicationPrefix(id)
		if ok != wantOK {
			t.Errorf("applicationPrefix(%q) ok=%v, want %v", id, ok, wantOK)
		}
	}
}

func TestDescendantPrefix_AppendsSlash(t *testing.T) {
	if got := descendantPrefix("can://go/app"); got != "can://go/app/" {
		t.Errorf("descendantPrefix = %q, want can://go/app/", got)
	}
}

func TestFinish_IsOrderStable(t *testing.T) {
	// Build the SAME logical graph (same nodes, same A→B and B→C edges) but add
	// nodes and edges in two different INSERTION orders; finish() must sort to
	// identical ordering both times.
	ref := func(b *RowBuilder, id string) NodeRef {
		return b.Node([]string{SymbolLabel, LabelCallable}, "id", id, nil, "")
	}
	const A, B, C = "can://go/app/a.go/A", "can://go/app/b.go/B", "can://go/app/c.go/C"

	build := func(reverse bool) GraphRows {
		b := NewRowBuilder()
		if reverse {
			// Insert nodes C,B,A and the B→C edge before the A→B edge.
			rc, rb, ra := ref(b, C), ref(b, B), ref(b, A)
			b.Edge(RelCalls, rb, rc, nil, "")
			b.Edge(RelCalls, ra, rb, nil, "")
		} else {
			ra, rb, rc := ref(b, A), ref(b, B), ref(b, C)
			b.Edge(RelCalls, ra, rb, nil, "")
			b.Edge(RelCalls, rb, rc, nil, "")
		}
		return b.Finish()
	}
	a := build(false)
	z := build(true)
	if !reflect.DeepEqual(nodeValues(a), nodeValues(z)) {
		t.Errorf("node order not stable:\n a=%v\n z=%v", nodeValues(a), nodeValues(z))
	}
	if !reflect.DeepEqual(edgeKeys(a), edgeKeys(z)) {
		t.Errorf("edge order not stable:\n a=%v\n z=%v", edgeKeys(a), edgeKeys(z))
	}
}

func TestCypherRendering(t *testing.T) {
	if got := cypherValue("a'b\\c"); got != `'a\'b\\c'` {
		t.Errorf("string escaping wrong: %s", got)
	}
	if got := cypherValue(true); got != "true" {
		t.Errorf("bool: %s", got)
	}
	if got := cypherValue([]string{"x", "y"}); got != "['x', 'y']" {
		t.Errorf("list: %s", got)
	}
	// Map keys sorted for determinism.
	if got := cypherMap(Props{"b": 1, "a": "x"}); got != "{a: 'x', b: 1}" {
		t.Errorf("map not sorted/deterministic: %s", got)
	}
	if got := cypherMap(nil); got != "{}" {
		t.Errorf("empty map: %s", got)
	}
}

// helpers.
func nodeValues(g GraphRows) []string {
	out := make([]string, len(g.Nodes))
	for i, n := range g.Nodes {
		out[i] = n.Value
	}
	return out
}
func edgeKeys(g GraphRows) []string {
	out := make([]string, len(g.Edges))
	for i, e := range g.Edges {
		out[i] = edgeSortKey(e)
	}
	return out
}
