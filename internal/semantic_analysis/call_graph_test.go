package semantic_analysis_test

import (
	"testing"

	"github.com/codellm-devkit/codeanalyzer-go/internal/schema"
	"github.com/codellm-devkit/codeanalyzer-go/internal/semantic_analysis"
)

func edge(src, tgt string, weight int, prov ...string) schema.GoCallEdge {
	return schema.GoCallEdge{Source: src, Target: tgt, Weight: weight, Provenance: prov}
}

// ── MergeEdges ────────────────────────────────────────────────────────────────

func TestMergeEdges_EmptyBoth(t *testing.T) {
	result := semantic_analysis.MergeEdges(nil, nil)
	if len(result) != 0 {
		t.Errorf("got %d edges, want 0", len(result))
	}
}

func TestMergeEdges_PrimaryOnly(t *testing.T) {
	primary := []schema.GoCallEdge{edge("a", "b", 1.0, "resolver")}
	result := semantic_analysis.MergeEdges(primary, nil)
	if len(result) != 1 {
		t.Fatalf("got %d edges, want 1", len(result))
	}
	if result[0].Source != "a" || result[0].Target != "b" {
		t.Errorf("unexpected edge: %+v", result[0])
	}
}

func TestMergeEdges_SecondaryOnly(t *testing.T) {
	secondary := []schema.GoCallEdge{edge("x", "y", 2.0, "codeql")}
	result := semantic_analysis.MergeEdges(nil, secondary)
	if len(result) != 1 {
		t.Fatalf("got %d edges, want 1", len(result))
	}
	if result[0].Source != "x" || result[0].Target != "y" {
		t.Errorf("unexpected edge: %+v", result[0])
	}
}

func TestMergeEdges_DisjointEdges(t *testing.T) {
	primary := []schema.GoCallEdge{edge("a", "b", 1.0, "resolver")}
	secondary := []schema.GoCallEdge{edge("c", "d", 1.0, "codeql")}
	result := semantic_analysis.MergeEdges(primary, secondary)
	if len(result) != 2 {
		t.Errorf("got %d edges, want 2", len(result))
	}
}

func TestMergeEdges_DuplicateAccumulatesWeight(t *testing.T) {
	primary := []schema.GoCallEdge{edge("a", "b", 3, "resolver")}
	secondary := []schema.GoCallEdge{edge("a", "b", 5, "codeql")}
	result := semantic_analysis.MergeEdges(primary, secondary)
	if len(result) != 1 {
		t.Fatalf("duplicate (a→b) should collapse to 1 edge; got %d", len(result))
	}
	if result[0].Weight != 8 {
		t.Errorf("weight: got %v, want 8", result[0].Weight)
	}
}

func TestMergeEdges_DuplicateUnionsProvenance(t *testing.T) {
	primary := []schema.GoCallEdge{edge("a", "b", 1.0, "resolver")}
	secondary := []schema.GoCallEdge{edge("a", "b", 1.0, "codeql")}
	result := semantic_analysis.MergeEdges(primary, secondary)
	if len(result) != 1 {
		t.Fatalf("got %d edges, want 1", len(result))
	}
	provSet := map[string]bool{}
	for _, p := range result[0].Provenance {
		provSet[p] = true
	}
	if !provSet["resolver"] || !provSet["codeql"] {
		t.Errorf("provenance union failed; got %v", result[0].Provenance)
	}
}

func TestMergeEdges_DuplicateProvenanceNotDuplicated(t *testing.T) {
	primary := []schema.GoCallEdge{edge("a", "b", 1.0, "resolver")}
	secondary := []schema.GoCallEdge{edge("a", "b", 1.0, "resolver")}
	result := semantic_analysis.MergeEdges(primary, secondary)
	if len(result) != 1 {
		t.Fatalf("got %d edges, want 1", len(result))
	}
	count := 0
	for _, p := range result[0].Provenance {
		if p == "resolver" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("duplicate provenance should appear once; got %d times", count)
	}
}

func TestMergeEdges_OrderPreserved(t *testing.T) {
	primary := []schema.GoCallEdge{
		edge("a", "b", 1.0),
		edge("c", "d", 1.0),
	}
	secondary := []schema.GoCallEdge{
		edge("e", "f", 1.0),
	}
	result := semantic_analysis.MergeEdges(primary, secondary)
	if len(result) != 3 {
		t.Fatalf("got %d edges, want 3", len(result))
	}
	// Primary edges come first, then secondary.
	if result[0].Source != "a" || result[1].Source != "c" || result[2].Source != "e" {
		t.Errorf("order not preserved: %v", result)
	}
}
