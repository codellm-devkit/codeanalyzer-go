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

// TestMergeEdges_KeepsDeclaringFilesApart: two inits (one signature, two
// files) calling one callee are two edges, and so are two VTA edges to the
// same-signature closures of two files' inits; the same file pair merges.
func TestMergeEdges_KeepsDeclaringFilesApart(t *testing.T) {
	at := func(e schema.GoCallEdge, src, dst string) schema.GoCallEdge {
		e.SourcePath, e.TargetPath = src, dst
		return e
	}
	primary := []schema.GoCallEdge{at(edge("p.init", "p.setup", 1, "go/types"), "a.go", "")}
	secondary := []schema.GoCallEdge{
		at(edge("p.init", "p.setup", 1, "go/types"), "b.go", ""),
		at(edge("p.Run", "p.init.closure_1", 1, "vta"), "a.go", "a.go"),
		at(edge("p.Run", "p.init.closure_1", 1, "vta"), "a.go", "b.go"),
		at(edge("p.init", "p.setup", 1, "vta"), "a.go", ""),
	}
	result := semantic_analysis.MergeEdges(primary, secondary)
	if len(result) != 4 {
		t.Fatalf("got %d edges, want 4: %+v", len(result), result)
	}
	if result[0].Weight != 2 || len(result[0].Provenance) != 2 {
		t.Errorf("a.go init edge = %+v, want weight 2, prov [go/types vta]", result[0])
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
	cg := semantic_analysis.NewCallGraphBuilder(dir, b.Fset(), b.Pkgs())
	edges := cg.Build(st)
	externals := cg.Externals()

	got := make([]string, 0, len(edges))
	for _, e := range edges {
		if _, ext := externals[e.Target]; ext {
			continue // the golden set locks in-project edges; library edges are tested apart
		}
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

// TestCallGraph_ClosureSitesResolve: call sites inside function literals are
// backfilled and produce edges whose source is the closure signature and whose
// SourcePath is the declaring file (#17).
func TestCallGraph_ClosureSitesResolve(t *testing.T) {
	dir, err := filepath.Abs("../../testdata/dataflow")
	if err != nil {
		t.Fatal(err)
	}
	b := syntactic_analysis.NewSymbolTableBuilder(dir)
	st, err := b.Build(nil, true)
	if err != nil {
		t.Fatalf("symbol table build failed: %v", err)
	}
	cg := semantic_analysis.NewCallGraphBuilder(dir, b.Fset(), b.Pkgs())
	edges := cg.Build(st)

	const outer = "example.com/dataflow/flow.Closure"
	lit, ok := st["flow/flow.go"].Functions[outer].InnerCallables["closure_1"]
	if !ok {
		t.Fatal("Closure has no closure_1")
	}
	if lit.Path != "flow/flow.go" {
		t.Errorf("closure Path = %q, want flow/flow.go", lit.Path)
	}
	if len(lit.CallSites) != 1 || lit.CallSites[0].CalleeSignature == nil ||
		*lit.CallSites[0].CalleeSignature != "example.com/dataflow/flow.f" {
		t.Fatalf("closure call site not resolved: %+v", lit.CallSites)
	}
	for _, e := range edges {
		if e.Source == outer+".closure_1" && e.Target == "example.com/dataflow/flow.f" {
			if e.SourcePath != "flow/flow.go" {
				t.Errorf("edge SourcePath = %q, want flow/flow.go", e.SourcePath)
			}
			return
		}
	}
	t.Error("no edge from the closure to flow.f")
}

// TestLibraryByPath: the no-source-file fallback classifies by the module
// path prefix when go.mod names one (so an unloaded project package is still
// project code), else by whether the package was loaded.
func TestLibraryByPath(t *testing.T) {
	for _, c := range []struct {
		pkg, mod string
		loaded   bool
		want     bool
	}{
		{"example.com/app", "example.com/app", false, false},
		{"example.com/app/internal/x", "example.com/app", false, false}, // unloaded project package
		{"example.com/application", "example.com/app", true, true},      // prefix is segment-wise
		{"fmt", "example.com/app", false, true},
		{"example.com/app/x", "", true, false}, // no go.mod: loaded = project
		{"example.com/app/x", "", false, true},
	} {
		if got := semantic_analysis.LibraryByPath(c.pkg, c.mod, c.loaded); got != c.want {
			t.Errorf("LibraryByPath(%q, %q, %v) = %v, want %v", c.pkg, c.mod, c.loaded, got, c.want)
		}
	}
}

// TestCallGraph_SamePositionInSiblingFiles: HelpX (types.go) and the method P
// (methods.go, filed under T in types.go) each make one call at the same
// line:col. Each must resolve its own callee; resolving by position over the
// whole package attributed right to HelpX (#32).
func TestCallGraph_SamePositionInSiblingFiles(t *testing.T) {
	dir, err := filepath.Abs("../../testdata/callsite_collision")
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
	want := []string{
		"example.com/callsitecollision.HelpX -> example.com/callsitecollision.wrong",
		"example.com/callsitecollision.T.P -> example.com/callsitecollision.right",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("edges = %q, want %q", got, want)
	}
}
