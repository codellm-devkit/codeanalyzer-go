package neo4j

import (
	"encoding/json"
	"testing"
)

// M1 schema-contract gate. These lock the declarative contract itself:
//   - the DERIVED DDL has exactly one uniqueness constraint per distinct
//     (mergeLabel, key), and the symbol family collapses to one;
//   - every relationship endpoint names a DECLARED node label (no edge wired to
//     a label the contract doesn't define);
//   - the --emit schema document round-trips through JSON.
//
// TestConformance_ProjectorEmitsOnlyDeclared is the anti-drift scaffold the M3
// projector fills: once project() exists, it walks a fixture at L2 and asserts
// the emitter never produces a label / relationship / property undeclared here.
// Until then it validates the contract is internally well-formed, so the gate
// is green and meaningful at M1.

// declaredLabels is the set of every node label the contract defines.
func declaredLabels() map[string]NodeSpec {
	m := make(map[string]NodeSpec, len(Nodes))
	for _, n := range Nodes {
		m[n.Label] = n
	}
	return m
}

func TestDDL_OneConstraintPerDistinctMergeKey(t *testing.T) {
	cs := UniquenessConstraints()

	// No duplicate (label, property) pairs.
	seen := make(map[string]bool)
	for _, c := range cs {
		key := c.Label + "." + c.Property
		if seen[key] {
			t.Errorf("duplicate uniqueness constraint for %s", key)
		}
		seen[key] = true
	}

	// Count must equal the number of DISTINCT (mergeLabel, key) pairs in Nodes.
	distinct := make(map[string]bool)
	for _, n := range Nodes {
		distinct[n.MergeLabel+"\x00"+n.Key] = true
	}
	if len(cs) != len(distinct) {
		t.Errorf("derived %d constraints, want %d distinct (mergeLabel,key) pairs", len(cs), len(distinct))
	}

	// The symbol family must collapse: GoType/GoCallable/GoExternal all merge on
	// GoSymbol, so there is exactly ONE GoSymbol.id constraint, not three.
	var goSymbol int
	for _, c := range cs {
		if c.Label == SymbolLabel {
			goSymbol++
		}
	}
	if goSymbol != 1 {
		t.Errorf("GoSymbol constraints = %d, want exactly 1 (the shared merge label collapses the family)", goSymbol)
	}
}

func TestContract_EveryRelEndpointIsDeclared(t *testing.T) {
	labels := declaredLabels()
	for _, r := range Rels {
		for _, l := range r.From {
			if _, ok := labels[l]; !ok {
				t.Errorf("relationship %s has undeclared From label %q", r.Type, l)
			}
		}
		for _, l := range r.To {
			if _, ok := labels[l]; !ok {
				t.Errorf("relationship %s has undeclared To label %q", r.Type, l)
			}
		}
		if len(r.From) == 0 || len(r.To) == 0 {
			t.Errorf("relationship %s has an empty endpoint set", r.Type)
		}
	}
}

func TestContract_CanNodesCarryIdKey(t *testing.T) {
	// Every can:// node keys on "id" (prefix scoping and the marker index depend
	// on it); neutral nodes may key otherwise but here also use "id".
	for _, n := range Nodes {
		if n.CanNode && n.Key != "id" {
			t.Errorf("can:// node %s keys on %q, want \"id\"", n.Label, n.Key)
		}
	}
}

func TestContract_PrefixesMatchSDKExpectations(t *testing.T) {
	// The SDK's GoAnalysisBackend sets N="Go", P="GO_"; the graph vocabulary and
	// the backend's N/P must agree letter-for-letter.
	if NodePrefix != "Go" || RelPrefix != "GO_" {
		t.Fatalf("prefixes drifted: NodePrefix=%q RelPrefix=%q, want \"Go\"/\"GO_\"", NodePrefix, RelPrefix)
	}
	for _, n := range Nodes {
		// Neutral layer is intentionally un-prefixed.
		if n.Label == LabelArtifact || n.Label == LabelPackage || n.Label == LabelConfigKey {
			continue
		}
		if len(n.Label) < 2 || n.Label[:2] != NodePrefix {
			t.Errorf("code node label %q is not %s-prefixed", n.Label, NodePrefix)
		}
	}
}

func TestSchemaDocument_RoundTrips(t *testing.T) {
	doc := BuildSchemaDocument()
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal schema document: %v", err)
	}
	var back SchemaDocument
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("unmarshal schema document: %v", err)
	}
	if back.SchemaVersion != SchemaVersion {
		t.Errorf("schema_version = %q, want %q", back.SchemaVersion, SchemaVersion)
	}
	if len(back.Nodes) != len(Nodes) || len(back.Relationships) != len(Rels) {
		t.Errorf("round-trip lost rows: nodes %d/%d, rels %d/%d",
			len(back.Nodes), len(Nodes), len(back.Relationships), len(Rels))
	}
	if len(back.Constraints) == 0 || len(back.Indexes) == 0 {
		t.Error("schema document missing derived DDL")
	}
}

// TestConformance_ProjectorEmitsOnlyDeclared is the M3 anti-drift scaffold.
// When project() lands, replace the skip with: project a fixture at L2, collect
// every (label, prop) and (relType) it emits, and assert each is declared here.
func TestConformance_ProjectorEmitsOnlyDeclared(t *testing.T) {
	t.Skip("scaffold: filled by M3 (project.go) — asserts the projector emits no undeclared label/relationship/property")
}
