package semantic_analysis_test

import (
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/codellm-devkit/codeanalyzer-go/internal/schema"
	"github.com/codellm-devkit/codeanalyzer-go/internal/semantic_analysis"
	"github.com/codellm-devkit/codeanalyzer-go/internal/syntactic_analysis"
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
	secondary := []schema.GoCallEdge{edge("x", "y", 2.0, "framework")}
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
	secondary := []schema.GoCallEdge{edge("c", "d", 1.0, "framework")}
	result := semantic_analysis.MergeEdges(primary, secondary)
	if len(result) != 2 {
		t.Errorf("got %d edges, want 2", len(result))
	}
}

func TestMergeEdges_DuplicateAccumulatesWeight(t *testing.T) {
	primary := []schema.GoCallEdge{edge("a", "b", 3, "resolver")}
	secondary := []schema.GoCallEdge{edge("a", "b", 5, "framework")}
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
	secondary := []schema.GoCallEdge{edge("a", "b", 1.0, "framework")}
	result := semantic_analysis.MergeEdges(primary, secondary)
	if len(result) != 1 {
		t.Fatalf("got %d edges, want 1", len(result))
	}
	provSet := map[string]bool{}
	for _, p := range result[0].Provenance {
		provSet[p] = true
	}
	if !provSet["resolver"] || !provSet["framework"] {
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

// ── Call-site index ───────────────────────────────────────────────────────────

// TestCallGraph_IndexMatchesWalk locks the chi edge set (see chiGoldenEdges for the delta vs the walk) after
// resolveCallSite switched from a per-site AST walk to the position index.
func TestCallGraph_IndexMatchesWalk(t *testing.T) {
	dir, err := filepath.Abs("../../testdata/chi")
	if err != nil {
		t.Fatal(err)
	}
	b := syntactic_analysis.NewSymbolTableBuilder(dir)
	st, err := b.Build(nil, true)
	if err != nil {
		t.Fatalf("symbol table build failed: %v", err)
	}
	edges := semantic_analysis.NewCallGraphBuilder(dir, b.Fset(), b.Pkgs()).Build(st)

	got := make([]string, 0, len(edges))
	for _, e := range edges {
		got = append(got, e.Source+" -> "+e.Target)
	}
	sort.Strings(got)

	if !reflect.DeepEqual(got, chiGoldenEdges) {
		t.Fatalf("edge set changed: got %d edges, want %d\nfirst diff: %s",
			len(got), len(chiGoldenEdges), firstDiff(got, chiGoldenEdges))
	}
}

func firstDiff(a, b []string) string {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return fmt.Sprintf("[%d] got %q want %q", i, a[i], b[i])
		}
	}
	return fmt.Sprintf("length differs (%d vs %d)", len(a), len(b))
}
