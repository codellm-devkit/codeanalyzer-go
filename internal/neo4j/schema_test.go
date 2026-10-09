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

// TestConformance_ProjectorEmitsOnlyDeclared is the anti-drift gate (filled by
// M3). It projects the multipackage fixture at L2 and asserts the real emitter
// never produces a label, relationship type, or property that schema.go does
// not declare — so the contract cannot silently drift from the projector.
func TestConformance_ProjectorEmitsOnlyDeclared(t *testing.T) {
	rows := Project(buildMultipackageL2(t))

	// Declared label set + per-label allowed property set. A node's labels[0] is
	// the MERGE label, which for the symbol family is the SHARED SymbolLabel; the
	// specific kind (GoType/GoCallable/GoExternal) is then an extra label. So the
	// NodeSpec is resolved by the specific kind label, and properties validate
	// against it.
	labelDecl := declaredLabels()
	validMerge := map[string]bool{MarkerLabel: true, SymbolLabel: true}
	for _, n := range Nodes {
		validMerge[n.MergeLabel] = true
	}

	for _, n := range rows.Nodes {
		// labels[0] must be a legitimate MERGE label (a node's own label, the
		// shared SymbolLabel, or — never — the marker).
		if !validMerge[n.Labels[0]] {
			t.Errorf("node MERGE label %q is undeclared", n.Labels[0])
			continue
		}
		// Resolve the spec by the specific kind label (labels[0] when it is the
		// node's own merge label, else the first extra label that is a declared kind).
		spec, ok := labelDecl[n.Labels[0]]
		if !ok {
			for _, l := range n.Labels[1:] {
				if s, found := labelDecl[l]; found {
					spec, ok = s, true
					break
				}
			}
		}
		if !ok {
			t.Errorf("node with labels %v matches no declared kind", n.Labels)
			continue
		}
		// Every extra label must be a declared kind or the marker.
		for _, l := range n.Labels[1:] {
			if _, isKind := labelDecl[l]; !isKind && l != MarkerLabel {
				t.Errorf("node carries undeclared extra label %q", l)
			}
		}
		allowed := map[string]bool{spec.Key: true}
		for _, pr := range spec.Props {
			allowed[pr] = true
		}
		for pk := range n.Props {
			if !allowed[pk] {
				t.Errorf("node %s carries undeclared property %q", spec.Label, pk)
			}
		}
	}

	relDecl := make(map[string]RelSpec, len(Rels))
	for _, r := range Rels {
		relDecl[r.Type] = r
	}
	for _, e := range rows.Edges {
		spec, ok := relDecl[e.Type]
		if !ok {
			t.Errorf("relationship type %q is undeclared", e.Type)
			continue
		}
		allowed := map[string]bool{}
		for _, pr := range spec.Props {
			allowed[pr] = true
		}
		for pk := range e.Props {
			if !allowed[pk] {
				t.Errorf("relationship %s carries undeclared property %q", e.Type, pk)
			}
		}
	}
}
