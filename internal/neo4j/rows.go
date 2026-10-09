package neo4j

import (
	"fmt"
	"sort"
	"strings"
)

// Milestone M2: the output-agnostic row IR and the RowBuilder. GraphRows is a
// deterministic, deduped bag of nodes and edges with NO I/O and NO driver; both
// writers (cypher.go, bolt.go) consume the identical bag. The RowBuilder is the
// in-memory analog of Neo4j MERGE semantics — it mirrors MERGE … SET += props
// before any writer runs, so each writer just replays a clean, deduped result.
//
// Property values must be Neo4j-legal: primitives or homogeneous arrays of
// primitives. nil is PRUNED — in Neo4j an absent property IS null, so
// "not carried" and "empty" never collide. Optional/variant-shaped fields are
// JSON-encoded into a *_json string property by the projector (M3), not here.

// Value is a Neo4j-legal property value: a primitive (string/int/bool) or a
// homogeneous slice of primitives. The projector is responsible for only ever
// passing legal values; the writers render whatever is here.
type Value any

// Props is a property bag. A nil entry is dropped by prune().
type Props map[string]Value

// NodeRef is how an edge addresses an endpoint: the (label, keyProp, value) an
// edge MATCHes on. Label is the MERGE label (labels[0]) of the target node.
type NodeRef struct {
	Label   string
	KeyProp string
	Value   string
}

// NodeRow is one graph node. Labels[0] is the constrained MERGE label; the rest
// are SET as extra labels (e.g. the :GoCanNode marker, the specific kind label
// alongside the shared :GoSymbol). Module is in-memory ONLY — the owning
// module's id, used by the incremental writer's per-module diff; it is NEVER
// emitted as a graph property.
type NodeRow struct {
	Labels  []string
	KeyProp string
	Value   string
	Props   Props
	Module  string
}

// EdgeRow is one typed relationship. Key is the optional _k relationship
// discriminant: when set, the MERGE is on {_k: Key} so legitimately-parallel
// edges between the same endpoint pair (a conditional's true/false CFG pair,
// per-(var,prov) DDG edges) survive instead of collapsing.
type EdgeRow struct {
	Type  string
	From  NodeRef
	To    NodeRef
	Props Props
	Key   string
}

// GraphRows is the finished, deterministic bag.
type GraphRows struct {
	Nodes []NodeRow
	Edges []EdgeRow
}

// ─── RowBuilder ──────────────────────────────────────────────────────────────

// RowBuilder accumulates nodes and edges with in-memory MERGE semantics. Nodes
// dedup on (labels[0], value); edges are append-only (per-pair facts that must
// not duplicate need an explicit guard in the projector). Edges whose target
// may be external/library code are DEFERRED and kept at finish() only if the
// target node was actually emitted — the edge-only-when-resolved rule, so an
// unresolved target degrades to a source-node property instead of a dangling edge.
type RowBuilder struct {
	nodeIndex map[string]int // (labels[0] + "\x00" + value) -> index into nodes
	nodes     []NodeRow
	edges     []EdgeRow
	deferred  []EdgeRow
}

// NewRowBuilder returns an empty builder.
func NewRowBuilder() *RowBuilder {
	return &RowBuilder{nodeIndex: make(map[string]int)}
}

// Node upserts a node keyed on (labels[0], value). Re-seeing the same identity
// MERGES props (last-write-wins) and UNIONs labels — a hot external symbol
// collapses to one row. Returns a NodeRef edges can address it by. nil props
// are pruned on the way in.
func (b *RowBuilder) Node(labels []string, keyProp, value string, props Props, module string) NodeRef {
	if len(labels) == 0 {
		panic("neo4j: Node requires at least one label (labels[0] is the MERGE label)")
	}
	k := labels[0] + "\x00" + value
	if i, ok := b.nodeIndex[k]; ok {
		existing := &b.nodes[i]
		existing.Labels = unionLabels(existing.Labels, labels)
		for pk, pv := range prune(props) {
			if existing.Props == nil {
				existing.Props = Props{}
			}
			existing.Props[pk] = pv
		}
		if existing.Module == "" {
			existing.Module = module
		}
		return NodeRef{Label: labels[0], KeyProp: keyProp, Value: value}
	}
	b.nodeIndex[k] = len(b.nodes)
	b.nodes = append(b.nodes, NodeRow{
		Labels:  append([]string(nil), labels...),
		KeyProp: keyProp,
		Value:   value,
		Props:   prune(props),
		Module:  module,
	})
	return NodeRef{Label: labels[0], KeyProp: keyProp, Value: value}
}

// Edge appends a relationship whose endpoints are known to exist this run.
// Append-only — not deduped.
func (b *RowBuilder) Edge(relType string, from, to NodeRef, props Props, key string) {
	b.edges = append(b.edges, EdgeRow{Type: relType, From: from, To: to, Props: prune(props), Key: key})
}

// EdgeToSymbol defers a relationship whose target (addressed by its can:// id)
// may be library/external code not present in the graph. At finish() the edge
// is kept ONLY if a node with that (label, value) was emitted — so EXTENDS/
// SATISFIES/RESOLVES_TO never dangle. targetLabel is the MERGE label the target
// would carry if present (e.g. SymbolLabel).
func (b *RowBuilder) EdgeToSymbol(relType string, from NodeRef, targetLabel, targetKey, targetValue string, props Props, key string) {
	b.deferred = append(b.deferred, EdgeRow{
		Type:  relType,
		From:  from,
		To:    NodeRef{Label: targetLabel, KeyProp: targetKey, Value: targetValue},
		Props: prune(props),
		Key:   key,
	})
}

// Finish resolves deferred edges (keeping only those whose target was emitted),
// sorts nodes and edges by a stable key, and returns the GraphRows. Determinism
// matters: identical input must yield byte-identical output downstream.
func (b *RowBuilder) Finish() GraphRows {
	emitted := make(map[string]bool, len(b.nodes))
	for _, n := range b.nodes {
		emitted[n.Labels[0]+"\x00"+n.Value] = true
	}
	for _, e := range b.deferred {
		if emitted[e.To.Label+"\x00"+e.To.Value] {
			b.edges = append(b.edges, e)
		}
	}

	sort.Slice(b.nodes, func(i, j int) bool {
		if b.nodes[i].Labels[0] != b.nodes[j].Labels[0] {
			return b.nodes[i].Labels[0] < b.nodes[j].Labels[0]
		}
		return b.nodes[i].Value < b.nodes[j].Value
	})
	sort.Slice(b.edges, func(i, j int) bool {
		return edgeSortKey(b.edges[i]) < edgeSortKey(b.edges[j])
	})

	return GraphRows{Nodes: b.nodes, Edges: b.edges}
}

// edgeSortKey is the total order for edges: type, then endpoints, then the _k
// discriminant (so parallel edges per pair order stably).
func edgeSortKey(e EdgeRow) string {
	return strings.Join([]string{e.Type, e.From.Label, e.From.Value, e.To.Label, e.To.Value, e.Key}, "\x00")
}

// ─── scoping helpers ──────────────────────────────────────────────────────────

// applicationPrefix returns the bare application id used to scope destructive
// statements. It REFUSES anything that is not a bare can://<lang>/<app> id (an
// empty or deeper id would silently widen or narrow the blast radius): it
// returns ("", false) so a hand-built GraphRows with no proper application root
// gets NO destructive statement. The app segment is the outermost, so one
// prefix spans every module the app owns and nothing another app owns.
func applicationPrefix(appID string) (string, bool) {
	const scheme = "can://"
	if !strings.HasPrefix(appID, scheme) {
		return "", false
	}
	rest := strings.TrimPrefix(appID, scheme)
	// A bare app id is exactly <lang>/<app> — two non-empty segments, no deeper.
	parts := strings.Split(rest, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", false
	}
	return appID, true
}

// descendantPrefix appends "/" to a can:// id so a STARTS WITH predicate matches
// strict descendants only — "foo.go" is not treated as a prefix of "foo.go2",
// and the app root id itself (can://go/app) is outside its own descendant
// prefix (can://go/app/), so a wipe must match the root separately.
func descendantPrefix(canID string) string {
	return canID + "/"
}

// ─── pruning & Cypher-literal rendering ───────────────────────────────────────

// prune drops nil-valued and empty-slice properties: in Neo4j an absent
// property is null, so a non-fact must simply not be written. Returns nil for
// an all-empty bag so callers can leave Props nil.
func prune(p Props) Props {
	if len(p) == 0 {
		return nil
	}
	out := make(Props, len(p))
	for k, v := range p {
		if v == nil {
			continue
		}
		if s, ok := v.([]string); ok && len(s) == 0 {
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// cypherValue renders a Neo4j-legal value as a Cypher literal (used by the
// snapshot writer; the Bolt writer passes values as parameters instead).
func cypherValue(v Value) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case string:
		return cypherString(x)
	case bool:
		if x {
			return "true"
		}
		return "false"
	case int:
		return fmt.Sprintf("%d", x)
	case int64:
		return fmt.Sprintf("%d", x)
	case []string:
		parts := make([]string, len(x))
		for i, s := range x {
			parts[i] = cypherString(s)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	default:
		// Fall back to a quoted Go rendering rather than emit something illegal.
		return cypherString(fmt.Sprintf("%v", x))
	}
}

// cypherString renders a Cypher single-quoted string literal with escaping.
func cypherString(s string) string {
	var b strings.Builder
	b.WriteByte('\'')
	for _, r := range s {
		switch r {
		case '\'':
			b.WriteString("\\'")
		case '\\':
			b.WriteString("\\\\")
		case '\n':
			b.WriteString("\\n")
		case '\r':
			b.WriteString("\\r")
		case '\t':
			b.WriteString("\\t")
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('\'')
	return b.String()
}

// cypherMap renders a Props bag as a Cypher map literal with sorted keys (so
// output is deterministic). An empty bag renders as "{}".
func cypherMap(p Props) string {
	if len(p) == 0 {
		return "{}"
	}
	keys := make([]string, 0, len(p))
	for k := range p {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+": "+cypherValue(p[k]))
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// unionLabels merges two label slices preserving order and de-duplicating,
// keeping labels[0] (the MERGE label) first.
func unionLabels(a, b []string) []string {
	seen := make(map[string]bool, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, l := range a {
		if !seen[l] {
			seen[l] = true
			out = append(out, l)
		}
	}
	for _, l := range b {
		if !seen[l] {
			seen[l] = true
			out = append(out, l)
		}
	}
	return out
}
