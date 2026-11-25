package neo4j

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"

	v2 "github.com/codellm-devkit/codeanalyzer-go/internal/schema/v2"
	"github.com/codellm-devkit/codeanalyzer-go/internal/syntactic_analysis/v2emit"
)

// collectStager records the stages in order.
type collectStager struct {
	stages   []string
	nodes    []NodeRow
	edgeRows []EdgeRow
	mods     []string
	endIDs   []string
}

func (c *collectStager) begin(app NodeRow) error {
	c.stages = append(c.stages, "begin")
	c.nodes = append(c.nodes, app)
	return nil
}

func (c *collectStager) shared(n []NodeRow) error {
	c.stages = append(c.stages, "shared")
	c.nodes = append(c.nodes, n...)
	return nil
}

func (c *collectStager) module(id string, n []NodeRow) error {
	c.stages = append(c.stages, "module")
	c.mods = append(c.mods, id)
	c.nodes = append(c.nodes, n...)
	return nil
}

func (c *collectStager) late(n []NodeRow) error {
	c.stages = append(c.stages, "late")
	c.nodes = append(c.nodes, n...)
	return nil
}

func (c *collectStager) edges(e []EdgeRow) error {
	c.stages = append(c.stages, "edges")
	c.edgeRows = append(c.edgeRows, e...)
	return nil
}

func (c *collectStager) end(ids []string) error {
	c.stages = append(c.stages, "end")
	c.endIDs = ids
	return nil
}

// parityFixtures are the trees the parity gate projects both ways.
func parityFixtures(t *testing.T) map[string]*v2.Analysis {
	t.Helper()
	fx := map[string]*v2.Analysis{
		"multipackage L2": buildMultipackageL2(t),
		"dataflow L1":     buildDataflowAt(t, 1),
		"dataflow L3":     buildDataflowL3(t),
		"dataflow L4":     buildDataflowL4(t),
	}
	for _, name := range []string{"chi", "generics", "storedlit", "rangefunc"} {
		fx[name+" L4"] = buildFixtureL4(t, name)
	}
	fx["repository"] = repoAnalysis()
	fx["cross-module"] = crossModuleAnalysis()
	return fx
}

// crossModuleAnalysis is a hand-built tree whose facts cross modules the way
// pass 1 cannot see: a.go's body callee and embedded type live in b.go, a.go's
// type embeds a call-graph ghost, and one external is reached by a body callee only.
func crossModuleAnalysis() *v2.Analysis {
	const app = "can://x/x"
	a, b := v2.ModuleID(app, "a.go"), v2.ModuleID(app, "b.go")
	typeB, iface := v2.TypeID(b, "B"), v2.TypeID(b, "I")
	f, g := v2.CallableID(a, "F()"), v2.CallableID(b, "G()")
	ghost := v2.ExternalID(app, "lib", "Q")    // call graph only
	bodyOnly := v2.ExternalID(app, "lib", "E") // body callee only
	return &v2.Analysis{
		IdentityVersion: 2, SchemaVersion: v2.SchemaVersion, Language: v2.Language, MaxLevel: 2,
		Application: v2.Application{
			IdentityVersion: 2, ID: app, Kind: "application",
			SymbolTable: map[string]v2.Module{
				"a.go": {ID: a, Kind: "module", Package: "x",
					Imports: []v2.Import{{Name: "b", Path: "b.go"}, {Name: "fmt", Path: "fmt"}},
					Types: map[string]v2.Type{"A": {ID: v2.TypeID(a, "A"), Kind: "struct",
						BaseTypes: []string{typeB, ghost, "raw.Unresolved"}, Interfaces: []string{iface}}},
					Functions: map[string]v2.Callable{"F()": {ID: f, Kind: "function", Signature: "F()",
						Body: map[string]v2.BodyNode{
							"0": {Kind: "call", Callee: &g},
							"1": {Kind: "call", Callee: new(bodyOnly)},
						}}},
				},
				"b.go": {ID: b, Kind: "module", Package: "x",
					Types: map[string]v2.Type{
						"B": {ID: typeB, Kind: "struct"},
						"I": {ID: iface, Kind: "interface"},
					},
					Functions: map[string]v2.Callable{"G()": {ID: g, Kind: "function", Signature: "G()"}},
				},
			},
			CallGraph: []v2.Edge{{Src: f, Dst: g, Weight: 1}, {Src: f, Dst: ghost, Weight: 1}},
			ExternalSymbols: map[string]v2.ExternalSymbol{
				bodyOnly: {ID: bodyOnly, Kind: "external", Name: "E", Module: "lib", Signature: "E"},
			},
		},
	}
}

// mergeRows collapses streamed rows the way one RowBuilder would: nodes on
// the builder key (labels union, props merged), duplicate edges on the Finish key.
func mergeRows(nodes []NodeRow, edges []EdgeRow) GraphRows {
	b := NewRowBuilder()
	for _, n := range nodes {
		b.Node(n.Labels, n.KeyProp, n.Value, n.Props, n.Module)
	}
	for _, e := range edges {
		b.Edge(e.Type, e.From, e.To, e.Props, e.Key)
	}
	return b.Finish()
}

// diffRows reports the first five missing and extra rows of each kind, "" when equal.
func diffRows(want, got GraphRows) string {
	nodeKey := func(n NodeRow) string {
		return fmt.Sprintf("%v %s %v module=%q", n.Labels, n.Value, n.Props, n.Module)
	}
	var out strings.Builder
	report := func(kind string, want, got []string) {
		w, g := map[string]bool{}, map[string]bool{}
		for _, k := range want {
			w[k] = true
		}
		for _, k := range got {
			g[k] = true
		}
		for _, side := range []struct {
			name     string
			from, in map[string]bool
			keys     []string
		}{{"missing", w, g, want}, {"extra", g, w, got}} {
			n := 0
			for _, k := range side.keys {
				if !side.in[k] {
					if n < 5 {
						fmt.Fprintf(&out, "%s %s: %s\n", side.name, kind, strings.ReplaceAll(k, "\x00", " | "))
					}
					n++
				}
			}
			if n > 5 {
				fmt.Fprintf(&out, "... %d more %s %s\n", n-5, side.name, kind)
			}
		}
	}
	var wn, gn, we, ge []string
	for _, n := range want.Nodes {
		wn = append(wn, nodeKey(n))
	}
	for _, n := range got.Nodes {
		gn = append(gn, nodeKey(n))
	}
	for _, e := range want.Edges {
		we = append(we, edgeSortKey(e))
	}
	for _, e := range got.Edges {
		ge = append(ge, edgeSortKey(e))
	}
	report("node", wn, gn)
	report("edge", we, ge)
	return out.String()
}

// TestStream_ParityWithProject: the streamed rows, merged and deduplicated, equal Project.
func TestStream_ParityWithProject(t *testing.T) {
	for name, a := range parityFixtures(t) {
		want := Project(a)
		c := &collectStager{}
		if _, _, err := streamGraph(treeSource(a), c); err != nil {
			t.Fatal(err)
		}
		got := mergeRows(c.nodes, c.edgeRows)
		if d := diffRows(want, got); d != "" {
			t.Errorf("%s: streamed rows differ from Project:\n%s", name, d)
		}
	}
}

// TestStream_ParityThroughEmitterStream: streamGraph over v2emit.Open equals Project(v2emit.Emit(...)).
func TestStream_ParityThroughEmitterStream(t *testing.T) {
	for _, name := range []string{"dataflow", "chi", "generics", "storedlit", "rangefunc"} {
		in := buildL4Inputs(t, name)
		want := Project(v2emit.Emit(in.app, in.identity, in.dir, 4, "neo4j-gate", 3, in.l3, in.l4))
		c := &collectStager{}
		src := v2emit.Open(in.app, in.identity, in.dir, 4, "neo4j-gate", 3, in.l3, in.l4)
		if _, _, err := streamGraph(src, c); err != nil {
			t.Fatal(err)
		}
		if d := diffRows(want, mergeRows(c.nodes, c.edgeRows)); d != "" {
			t.Errorf("%s: streamed rows differ from Project(Emit):\n%s", name, d)
		}
	}
}

// TestStream_StageOrder: every node stage runs before the first edge stage.
func TestStream_StageOrder(t *testing.T) {
	a := buildDataflowL4(t)
	c := &collectStager{}
	if _, _, err := streamGraph(treeSource(a), c); err != nil {
		t.Fatal(err)
	}
	s := strings.Join(c.stages, ",")
	if !regexp.MustCompile(`^begin,shared,(module,)+late,(edges,)+end$`).MatchString(s) {
		t.Errorf("stages %s", s)
	}
	if !slices.IsSorted(c.mods) || len(c.mods) != len(a.Application.SymbolTable) {
		t.Errorf("modules %v", c.mods)
	}
	if !slices.Equal(c.endIDs, c.mods) {
		t.Errorf("end ids %v, module ids %v", c.endIDs, c.mods)
	}
	for _, n := range c.nodes[1:] { // c.nodes[0] is the begin row
		if n.Labels[0] == LabelApplication {
			if _, ok := n.Props["degraded_json"]; ok != (len(a.Degraded) > 0) {
				t.Errorf("late application row degraded_json present=%v, degraded=%d", ok, len(a.Degraded))
			}
		}
	}
}

// TestStream_CrossModuleFixtureHasTheHardRows keeps the cross-module parity
// fixture honest: Project must make each row that pass 1 alone would get wrong.
func TestStream_CrossModuleFixtureHasTheHardRows(t *testing.T) {
	rows := Project(crossModuleAnalysis())
	got := map[string]int{}
	for _, e := range rows.Edges {
		got[e.Type+" "+lastSegment(e.To.Value)]++
	}
	for _, want := range []string{
		RelEmbeds + " B", RelEmbeds + " Q", RelSatisfies + " I",
		RelResolvesTo + " G()", RelResolvesTo + " E", RelImports + " b.go", RelImports + " fmt",
	} {
		if got[want] != 1 {
			t.Errorf("want one %q edge, got %d", want, got[want])
		}
	}
	for _, n := range rows.Nodes {
		if hasLabel(n, LabelExternal) && lastSegment(n.Value) == "G()" {
			t.Errorf("false ghost for the declared callee %s", n.Value)
		}
	}
}

// TestReplayRows_SplitsAReadyGraphRows: replayRows sends every row once, in
// stage order, with the stand-in vertices in late and the modules sorted.
func TestReplayRows_SplitsAReadyGraphRows(t *testing.T) {
	a := buildDataflowL4(t)
	rows := Project(a)
	c := &collectStager{}
	nodes, edges, err := replayRows(rows, c)
	if err != nil {
		t.Fatal(err)
	}
	if nodes != len(rows.Nodes) || edges != len(rows.Edges) {
		t.Errorf("counts %d/%d, want %d/%d", nodes, edges, len(rows.Nodes), len(rows.Edges))
	}
	if s := strings.Join(c.stages, ","); !regexp.MustCompile(`^begin,shared,(module,)+late,edges,end$`).MatchString(s) {
		t.Errorf("stages %s", s)
	}
	if c.nodes[0].Labels[0] != LabelApplication || !slices.IsSorted(c.mods) || !slices.Equal(c.endIDs, c.mods) {
		t.Errorf("begin row %v, modules %v, end ids %v", c.nodes[0].Labels, c.mods, c.endIDs)
	}
	if d := diffRows(rows, mergeRows(c.nodes, c.edgeRows)); d != "" {
		t.Errorf("replayed rows differ:\n%s", d)
	}
}
