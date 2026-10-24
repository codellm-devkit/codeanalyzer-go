package neo4j

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/codellm-devkit/codeanalyzer-go/internal/dataflow"
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
	app := &schema.GoApplication{SymbolTable: st, CallGraph: edges, Externals: cg.Externals()}
	return v2emit.Emit(app, "multipackage", dir, 2, "neo4j-gate", 0, nil)
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

// TestProject_ExternalGhostCarriesSignature locks that a library callee becomes a
// :GoExternal ghost carrying signature/module, is the target of a GO_CALLS edge,
// and is reached by a GO_RESOLVES_TO edge from the call body node.
func TestProject_ExternalGhostCarriesSignature(t *testing.T) {
	a := buildMultipackageL2(t)
	rows := Project(a)
	ext := v2.ExternalID(a.Application.ID, "fmt", "Println")

	var ghost *NodeRow
	for i, n := range rows.Nodes {
		if n.Value == ext && hasLabel(n, LabelExternal) {
			ghost = &rows.Nodes[i]
		}
	}
	if ghost == nil {
		t.Fatalf("no :GoExternal ghost for %s", ext)
	}
	if ghost.Props["signature"] != "Println" || ghost.Props["module"] != "fmt" {
		t.Errorf("ghost props = %v, want signature=Println module=fmt", ghost.Props)
	}
	var calls, resolves bool
	for _, e := range rows.Edges {
		if e.To.Value != ext {
			continue
		}
		switch e.Type {
		case RelCalls:
			calls = true
		case RelResolvesTo:
			resolves = true
		}
	}
	if !calls || !resolves {
		t.Errorf("edges to external: GO_CALLS=%v GO_RESOLVES_TO=%v, want both", calls, resolves)
	}
}

// ─── L3 overlay ──────────────────────────────────────────────────────────────

// buildDataflowL3 builds testdata/dataflow through the real -a 3 wiring (symbol
// table, resolver, dataflow driver) and emits the v2 L3 analysis.
func buildDataflowL3(t *testing.T) *v2.Analysis {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	dir, err := filepath.Abs(filepath.Join(filepath.Dir(thisFile), "..", "..", "testdata", "dataflow"))
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
	res := dataflow.Run(b.Fset(), b.L3Units(st), 3, 4)
	return v2emit.Emit(app, "dataflow", dir, 3, "neo4j-gate", 3, res)
}

// walkCallables visits every callable (methods and nested closures included).
func walkCallables(a *v2.Analysis, visit func(c v2.Callable)) {
	var walk func(c v2.Callable)
	walk = func(c v2.Callable) {
		visit(c)
		for _, in := range c.Callables {
			walk(in)
		}
	}
	for _, m := range a.Application.SymbolTable {
		for _, ty := range m.Types {
			for _, c := range ty.Callables {
				walk(c)
			}
		}
		for _, f := range m.Functions {
			walk(f)
		}
	}
}

func countEdges(rows GraphRows, typ string) int {
	n := 0
	for _, e := range rows.Edges {
		if e.Type == typ {
			n++
		}
	}
	return n
}

// TestProject_L3CountsMatchJSON is the cross-projection gate: the graph carries
// exactly the facts the JSON carries.
func TestProject_L3CountsMatchJSON(t *testing.T) {
	a := buildDataflowL3(t)
	rows := Project(a)

	var cfg, cdg, ddg, body int
	walkCallables(a, func(c v2.Callable) {
		cfg += len(c.Cfg)
		cdg += len(c.Cdg)
		ddg += len(c.Ddg)
		body += len(c.Body)
	})
	if cfg == 0 || cdg == 0 || ddg == 0 {
		t.Fatalf("fixture produced no overlay edges: cfg=%d cdg=%d ddg=%d", cfg, cdg, ddg)
	}
	t.Logf("JSON: cfg=%d cdg=%d ddg=%d body=%d", cfg, cdg, ddg, body)

	for _, c := range []struct {
		name string
		got  int
		want int
	}{
		{RelCFGNext, countEdges(rows, RelCFGNext), cfg},
		{RelCDG, countEdges(rows, RelCDG), cdg},
		{RelDDG, countEdges(rows, RelDDG), ddg},
		{RelHasBodyNode, countEdges(rows, RelHasBodyNode), body},
	} {
		if c.got != c.want {
			t.Errorf("%s rows = %d, JSON = %d", c.name, c.got, c.want)
		}
	}
	nodes := 0
	for _, n := range rows.Nodes {
		if n.Labels[0] == LabelBodyNode {
			nodes++
		}
	}
	if nodes != body {
		t.Errorf(":GoBodyNode rows = %d, Σ len(body) = %d", nodes, body)
	}
}

// TestProject_L3OverlayEndpointsAreBodyNodes: every overlay endpoint addresses a
// projected :GoBodyNode by its global id.
func TestProject_L3OverlayEndpointsAreBodyNodes(t *testing.T) {
	rows := Project(buildDataflowL3(t))
	have := map[string]bool{}
	for _, n := range rows.Nodes {
		if n.Labels[0] == LabelBodyNode {
			have[n.Value] = true
		}
	}
	for _, e := range rows.Edges {
		switch e.Type {
		case RelCFGNext, RelCDG, RelDDG:
			for _, r := range []NodeRef{e.From, e.To} {
				if r.Label != LabelBodyNode || r.KeyProp != "id" || !have[r.Value] {
					t.Fatalf("%s endpoint %+v is not a projected :GoBodyNode", e.Type, r)
				}
			}
		}
	}
}

// TestProject_SyntheticBodyNodes locks the Task 11 gap: @entry/@exit are
// projected with the single-@ id, no span, and a GO_HAS_BODY_NODE edge.
func TestProject_SyntheticBodyNodes(t *testing.T) {
	a := buildDataflowL3(t)
	rows := Project(a)
	nodes := map[string]NodeRow{}
	for _, n := range rows.Nodes {
		if n.Labels[0] == LabelBodyNode {
			nodes[n.Value] = n
		}
	}
	hasEdge := map[string]string{} // body id -> owning callable id
	for _, e := range rows.Edges {
		if e.Type == RelHasBodyNode {
			hasEdge[e.To.Value] = e.From.Value
		}
	}
	checked := 0
	walkCallables(a, func(c v2.Callable) {
		for _, tag := range []string{"@entry", "@exit"} {
			if _, ok := c.Body[tag]; !ok {
				continue
			}
			id := c.ID + tag
			n, ok := nodes[id]
			if !ok {
				t.Errorf("synthetic node %s not projected", id)
				continue
			}
			if strings.Contains(id, "@@") {
				t.Errorf("doubled @ in %s", id)
			}
			if _, has := n.Props["span_json"]; has {
				t.Errorf("%s carries span_json", id)
			}
			if hasEdge[id] != c.ID {
				t.Errorf("%s: GO_HAS_BODY_NODE from %q, want %q", id, hasEdge[id], c.ID)
			}
			checked++
		}
	})
	if checked == 0 {
		t.Fatal("no synthetic nodes in the fixture")
	}
}

func overlayFixture() *v2.Analysis {
	const app = "can://go/x"
	const mod = app + "/a.go"
	const fn = mod + "/f()"
	return &v2.Analysis{
		SchemaVersion: v2.SchemaVersion, Language: v2.Language, MaxLevel: 3,
		Application: v2.Application{
			ID: app,
			SymbolTable: map[string]v2.Module{"a.go": {
				ID: mod,
				Functions: map[string]v2.Callable{"f()": {
					ID: fn,
					Body: map[string]v2.BodyNode{
						"@entry": {Kind: "entry", ID: fn + "@entry"},
						"3:2":    {Kind: "branch", ID: fn + "@3:2", Span: v2.Span{Start: [2]int{3, 2}, End: [2]int{3, 9}, Bytes: [2]int{10, 17}}},
						"4:3":    {Kind: "call", ID: fn + "@4:3", Parent: "3:2", Span: v2.Span{Start: [2]int{4, 3}, End: [2]int{4, 9}, Bytes: [2]int{20, 26}}},
						"@exit":  {Kind: "exit", ID: fn + "@exit"},
					},
					Cfg: []v2.CfgEdge{
						{Src: "3:2", Dst: "4:3", Kind: "true"},
						{Src: "3:2", Dst: "4:3", Kind: "exception"},
					},
					Cdg: []v2.CdgEdge{{Src: "3:2", Dst: "4:3"}},
					Ddg: []v2.DdgEdge{
						{Src: "@entry", Dst: "4:3", Var: "x", Prov: []string{"ssa"}},
						{Src: "@entry", Dst: "4:3", Var: "x", Prov: []string{"ssa", "points-to"}},
						{Src: "@entry", Dst: "4:3", Var: "y", Prov: []string{"ssa"}},
					},
				}},
			}},
		},
	}
}

func TestProject_ParallelCfgEdgesSurviveMerge(t *testing.T) {
	rows := Project(overlayFixture())
	keys := map[string]bool{}
	for _, e := range rows.Edges {
		if e.Type == RelCFGNext {
			if e.Key != e.Props["kind"] {
				t.Errorf("_k = %q, want kind %v", e.Key, e.Props["kind"])
			}
			keys[e.Key] = true
		}
	}
	if len(keys) != 2 || !keys["true"] || !keys["exception"] {
		t.Errorf("want parallel cfg rows true+exception, got %v", keys)
	}
}

func TestProject_DdgKeyIsVarAndProv(t *testing.T) {
	rows := Project(overlayFixture())
	got := map[string]bool{}
	for _, e := range rows.Edges {
		if e.Type != RelDDG {
			continue
		}
		prov, ok := e.Props["prov"].([]string)
		if !ok {
			t.Fatalf("prov = %T, want []string", e.Props["prov"])
		}
		if want := e.Props["var"].(string) + "|" + strings.Join(prov, ","); e.Key != want {
			t.Errorf("_k = %q, want %q", e.Key, want)
		}
		got[e.Key] = true
	}
	for _, k := range []string{"x|ssa", "x|ssa,points-to", "y|ssa"} {
		if !got[k] {
			t.Errorf("missing ddg key %q (got %v)", k, got)
		}
	}
	if len(got) != 3 {
		t.Errorf("want 3 distinct ddg keys, got %v", got)
	}
}

func TestProject_BodyNodeCallNodeAndVar(t *testing.T) {
	rows := Project(overlayFixture())
	const fn = "can://go/x/a.go/f()"
	for _, n := range rows.Nodes {
		if n.Labels[0] != LabelBodyNode {
			continue
		}
		_, hasVar := n.Props["var"]
		if hasVar {
			t.Errorf("%s carries var at L3", n.Value)
		}
		cn, hasCall := n.Props["call_node"]
		if n.Value == fn+"@4:3" {
			if cn != fn+"@3:2" {
				t.Errorf("call_node = %v, want %s@3:2", cn, fn)
			}
		} else if hasCall {
			t.Errorf("%s has unexpected call_node", n.Value)
		}
	}
}
