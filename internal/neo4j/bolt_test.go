package neo4j

import (
	"strings"
	"testing"
)

// M5 unit gate (DB-free): the pure pieces of the Bolt writer — node
// partitioning, module-hash extraction, param marshalling, and the critical
// invariant that every destructive query is scoped on :GoCanNode + the can://
// prefix (never an unqualified DELETE). The live-DB behavior (diff, additive
// vs eager) is exercised by bolt_integration_test.go behind the `neo4j_it`
// build tag.

func TestPartitionNodes_SharedHaveNoModule(t *testing.T) {
	b := NewRowBuilder()
	b.Node([]string{LabelModule, MarkerLabel}, "id", "can://go/app/x.go", Props{"content_hash": "h1"}, "x.go")
	b.Node([]string{LabelCallable, SymbolLabel, MarkerLabel}, "id", "can://go/app/x.go/f", nil, "x.go")
	b.Node([]string{SymbolLabel, LabelExternal, MarkerLabel}, "id", "can://go/app/@external/fmt", nil, "") // shared
	rows := b.Finish()

	byModule, shared := partitionNodes(rows.Nodes)
	if len(byModule["x.go"]) != 2 {
		t.Errorf("module x.go should own 2 nodes, got %d", len(byModule["x.go"]))
	}
	if len(shared) != 1 {
		t.Errorf("expected 1 shared (no-module) node, got %d", len(shared))
	}
}

func TestModuleHash_ReadsModuleNode(t *testing.T) {
	nodes := []NodeRow{
		{Labels: []string{LabelCallable}, Value: "can://go/app/x.go/f"},
		{Labels: []string{LabelModule}, Value: "can://go/app/x.go", Props: Props{"content_hash": "abc123"}},
	}
	if got := moduleHash(nodes); got != "abc123" {
		t.Errorf("moduleHash = %q, want abc123", got)
	}
	if got := moduleHash(nodes[:1]); got != "" {
		t.Errorf("moduleHash with no module node = %q, want empty", got)
	}
}

func TestDestructiveQueries_AreScopedAndAnchored(t *testing.T) {
	// Both destructive queries MUST filter on :GoCanNode and bind the prefix as a
	// parameter — never an unqualified DETACH DELETE.
	eq, ep := eagerPurgeQuery("can://go/app")
	assertScoped(t, "eagerPurge", eq, ep)
	if ep["p"] != "can://go/app/" || ep["root"] != "can://go/app" {
		t.Errorf("eagerPurge params not the app prefix+root: %v", ep)
	}

	pq, pp := purgeModuleQuery("can://go/app/x.go")
	assertScoped(t, "purgeModule", pq, pp)
	if pp["p"] != "can://go/app/x.go/" || pp["m"] != "can://go/app/x.go" {
		t.Errorf("purgeModule params not the module prefix+id: %v", pp)
	}
}

func assertScoped(t *testing.T, name, q string, params map[string]any) {
	t.Helper()
	if !strings.Contains(q, "DETACH DELETE") {
		t.Fatalf("%s: not a destructive query?", name)
	}
	if !strings.Contains(q, ":GoCanNode") {
		t.Errorf("%s: destructive query not anchored on :GoCanNode: %q", name, q)
	}
	if !strings.Contains(q, "n.id STARTS WITH $p") {
		t.Errorf("%s: destructive query not prefix-scoped on a bound param: %q", name, q)
	}
	// The prefix param must end in "/" (strict descendant containment).
	if p, _ := params["p"].(string); !strings.HasSuffix(p, "/") {
		t.Errorf("%s: prefix param %q must end in / for strict containment", name, p)
	}
}

func TestParamMarshalling(t *testing.T) {
	g := []NodeRow{{Value: "id1", Props: Props{"kind": "struct", "n": 3}}}
	ps := nodeParams(g)
	if ps[0]["k"] != "id1" {
		t.Errorf("node key not marshalled: %v", ps[0])
	}
	p := ps[0]["p"].(map[string]any)
	if p["kind"] != "struct" || p["n"] != 3 {
		t.Errorf("node props not marshalled: %v", p)
	}

	e := []EdgeRow{{From: NodeRef{Value: "a"}, To: NodeRef{Value: "b"}, Key: "kind", Props: Props{"var": "x"}}}
	ep := edgeParams(e, true)
	if ep[0]["a"] != "a" || ep[0]["b"] != "b" || ep[0]["k"] != "kind" {
		t.Errorf("edge endpoints/key not marshalled: %v", ep[0])
	}
	// Non-keyed edges omit k.
	ep2 := edgeParams(e, false)
	if _, ok := ep2[0]["k"]; ok {
		t.Error("non-keyed edge must not carry a _k param")
	}
}

func TestBoltConfig_LazyIsDefault(t *testing.T) {
	// Documents the invariant: a zero-value BoltConfig is lazy (Eager false), so
	// the default push is purely additive.
	var cfg BoltConfig
	if cfg.Eager {
		t.Error("zero-value BoltConfig must be lazy (Eager=false) — additive by default")
	}
}
