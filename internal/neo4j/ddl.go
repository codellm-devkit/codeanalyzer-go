package neo4j

import (
	"fmt"
	"sort"

	v2 "github.com/codellm-devkit/codeanalyzer-go/internal/schema/v2"
)

// Derived DDL + the --emit schema document. Constraints are DERIVED from the
// node specs — one uniqueness constraint per distinct (mergeLabel, key) — so a
// new label automatically brings its own constraint and there is no second list
// to keep in sync. Indexes are curated: the critical one is the :GoCanNode id
// range index that makes every scoped/destructive statement a prefix SEEK
// instead of a store scan (STARTS WITH is index-backed; CONTAINS/ENDS WITH are
// not).

// Constraint is one uniqueness constraint: UNIQUE on (Label, Property).
type Constraint struct {
	Name     string `json:"name"`
	Label    string `json:"label"`
	Property string `json:"property"`
}

// Index is one curated index.
type Index struct {
	Name     string `json:"name"`
	Label    string `json:"label"`
	Property string `json:"property"`
	// Kind is "range" (the default, backs prefix seeks) today; fulltext is
	// reserved for a later code-search index.
	Kind string `json:"kind"`
}

// UniquenessConstraints returns one constraint per distinct (mergeLabel, key),
// derived from Nodes. Deterministic: sorted by (label, property). The symbol
// family (:GoType/:GoCallable/:GoExternal all merge on :GoSymbol) collapses to
// a SINGLE GoSymbol.id constraint, which is the point of the shared merge label.
func UniquenessConstraints() []Constraint {
	seen := make(map[string]Constraint)
	for _, n := range Nodes {
		key := n.MergeLabel + "\x00" + n.Key
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = Constraint{
			Name:     constraintName(n.MergeLabel, n.Key),
			Label:    n.MergeLabel,
			Property: n.Key,
		}
	}
	out := make([]Constraint, 0, len(seen))
	for _, c := range seen {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Label != out[j].Label {
			return out[i].Label < out[j].Label
		}
		return out[i].Property < out[j].Property
	})
	return out
}

// Indexes returns the curated indexes. The id range index is anchored on the
// :GoCanNode marker label (property indexes are label-scoped, so the prefix
// predicate needs a label to seek on). cannode_kind supports kind filters over
// body/type nodes.
func Indexes() []Index {
	return []Index{
		{Name: "gocannode_id", Label: MarkerLabel, Property: "id", Kind: "range"},
		{Name: "gocallable_signature", Label: LabelCallable, Property: "signature", Kind: "range"},
	}
}

// constraintName builds a stable, lowercase constraint name, e.g.
// "gosymbol_id_unique".
func constraintName(label, prop string) string {
	return fmt.Sprintf("%s_%s_unique", lower(label), prop)
}

// ─── --emit schema document ──────────────────────────────────────────────────

// SchemaDocument is the machine-readable graph contract, serialized by
// --emit schema to schema.neo4j.json. It is the graph-side sibling of the
// keystone JSON schema and what the SDK's Neo4j backend reconstructs against.
type SchemaDocument struct {
	SchemaVersion string       `json:"schema_version"`
	Language      string       `json:"language"`
	NodePrefix    string       `json:"node_prefix"`
	RelPrefix     string       `json:"rel_prefix"`
	MarkerLabel   string       `json:"marker_label"`
	SymbolLabel   string       `json:"symbol_label"`
	Nodes         []NodeDoc    `json:"nodes"`
	Relationships []RelDoc     `json:"relationships"`
	Constraints   []Constraint `json:"constraints"`
	Indexes       []Index      `json:"indexes"`
}

// NodeDoc / RelDoc are the serialized (JSON-stable) views of NodeSpec / RelSpec.
type NodeDoc struct {
	Label      string   `json:"label"`
	MergeLabel string   `json:"merge_label"`
	Key        string   `json:"key"`
	Props      []string `json:"props"`
	CanNode    bool     `json:"can_node"`
}

type RelDoc struct {
	Type         string   `json:"type"`
	From         []string `json:"from"`
	To           []string `json:"to"`
	Props        []string `json:"props,omitempty"`
	Discriminant string   `json:"discriminant,omitempty"`
	Deferred     bool     `json:"deferred,omitempty"`
	Reserved     bool     `json:"reserved,omitempty"`
}

// BuildSchemaDocument assembles the full contract document from the in-repo
// declarations. It needs no analyzed project — --emit schema is static.
func BuildSchemaDocument() SchemaDocument {
	nodes := make([]NodeDoc, 0, len(Nodes))
	for _, n := range Nodes {
		nodes = append(nodes, NodeDoc(n))
	}
	rels := make([]RelDoc, 0, len(Rels))
	for _, r := range Rels {
		rels = append(rels, RelDoc(r))
	}
	return SchemaDocument{
		SchemaVersion: SchemaVersion,
		Language:      v2.Language,
		NodePrefix:    NodePrefix,
		RelPrefix:     RelPrefix,
		MarkerLabel:   MarkerLabel,
		SymbolLabel:   SymbolLabel,
		Nodes:         nodes,
		Relationships: rels,
		Constraints:   UniquenessConstraints(),
		Indexes:       Indexes(),
	}
}

// lower is a tiny ASCII-lowercase for constraint names (labels are ASCII).
func lower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}
