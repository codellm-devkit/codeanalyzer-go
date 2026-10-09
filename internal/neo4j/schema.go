// Package neo4j is the Neo4j projection of the canonical schema-v2 CPG envelope:
// a pure, identity-preserving second projection of the SAME tree the JSON path
// emits to analysis.json, keyed on the SAME can:// ids. JSON and graph are
// interchangeable and can never diverge.
//
// Module split (pure projection, dumb writers — mirrors codeanalyzer-python's
// codeanalyzer/neo4j/* and codeanalyzer-typescript's src/build/neo4j/*):
//
//	schema.go   this file — the declarative contract (labels, relationships,
//	            derived DDL, SCHEMA_VERSION, --emit schema serialization)
//	rows.go     output-agnostic row IR + RowBuilder (M2)
//	project.go  pure projector: v2.Analysis tree -> GraphRows (M3)
//	cypher.go   snapshot writer -> graph.cypher (M4)
//	bolt.go     incremental Bolt writer (M5)
//	emit.go     facade: EmitSchema / EmitNeo4j (M6)
//
// This file is the single in-repo source of truth for the graph vocabulary.
// Three things derive from it: the DDL (one uniqueness constraint per distinct
// (mergeLabel, key)); the --emit schema document; and the conformance test,
// which asserts the projector (M3) never emits an undeclared label /
// relationship / property. The contract therefore cannot silently drift from
// the emitter.
//
// Vocabulary decisions are recorded in CLAUDE.md § Neo4j-projection decisions
// and docs/design/Neo4j_projection/specs/neo4j-projection.md. The parity clause
// governs: GO_/Go-prefixed code vocabulary (leaf additions), the neutral
// Artifact/Package/ConfigKey layer reused verbatim as cross-language merge
// targets, never a renamed shared name.
package neo4j

import v2 "github.com/codellm-devkit/codeanalyzer-go/internal/schema/v2"

// SchemaVersion is the graph contract version, stamped onto the :GoApplication
// node so a consumer can detect a producer/consumer mismatch at runtime. It
// moves in lockstep with the JSON schema major (v2.SchemaVersion). The
// additive-MINOR rule is suspended for the 2.0.0 line (parity with the sibling
// ruling); consumers gate on the analyzer-version floor instead.
const SchemaVersion = v2.SchemaVersion // "2.0.0"

// ─── namespacing (parity: Python PY_/Py, TS TS_/TS) ─────────────────────────

// NodePrefix / RelPrefix are the node-label and relationship-type prefixes for
// Go's code vocabulary. The SDK's AnalysisBackend ABC reads these as its N / P
// ClassVars, so GoAnalysisBackend sets N="Go", P="GO_" — the graph vocabulary
// and the backend's N/P must agree letter-for-letter.
const (
	NodePrefix = "Go"
	RelPrefix  = "GO_"
)

// ─── markers & shared merge labels ──────────────────────────────────────────

const (
	// MarkerLabel rides every node keyed by a can:// id. It is a pure index
	// anchor — Neo4j property indexes are label-scoped, so the prefix predicate
	// `id STARTS WITH $p` can seek instead of scanning the store. It carries no
	// safety claim of its own (the test is the can:// scheme).
	MarkerLabel = "GoCanNode"

	// SymbolLabel is the shared MERGE label on :GoType / :GoCallable /
	// :GoExternal. One uniqueness constraint for the family, and an edge can
	// MATCH any of them by a single (GoSymbol {id}) lookup regardless of kind.
	SymbolLabel = "GoSymbol"
)

// ─── node labels ────────────────────────────────────────────────────────────

// Node labels. The Go set is deliberately NARROWER than Python's: Go has no
// decorators, no class attributes, and no module/callable-level variable nodes,
// so :GoDecorator / :GoAttribute / :GoVariable are absent by design (CLAUDE.md
// decision X1). Struct fields are the one leaf node (:GoField).
const (
	LabelApplication = "GoApplication"
	LabelModule      = "GoModule"
	LabelType        = "GoType" // kind ∈ struct|interface|alias|defined is a PROPERTY, not a label
	LabelCallable    = "GoCallable"
	LabelField       = "GoField"
	LabelBodyNode    = "GoBodyNode"
	LabelExternal    = "GoExternal"

	// Neutral repository-artifact layer — UN-prefixed by design: these are
	// cross-language merge targets, so a sibling analyzer over the same repo
	// lands on the SAME node instead of a per-language duplicate.
	LabelArtifact  = "Artifact"
	LabelPackage   = "Package"
	LabelConfigKey = "ConfigKey"
)

// ─── relationship types ─────────────────────────────────────────────────────

const (
	// Containment (the tree, rendered as edges).
	RelHasModule   = "GO_HAS_MODULE"
	RelDeclares    = "GO_DECLARES"   // module→func, callable→closure
	RelHasMethod   = "GO_HAS_METHOD" // type→receiver-method
	RelHasField    = "GO_HAS_FIELD"
	RelHasBodyNode = "GO_HAS_BODY_NODE"

	// Call graph.
	RelCalls      = "GO_CALLS"       // weight, prov; endpoints resolve or fall back to an external ghost
	RelResolvesTo = "GO_RESOLVES_TO" // call body node → callee

	// Inheritance — Go's two distinct mechanisms, named honestly (CLAUDE.md T2):
	// GO_EMBEDS = explicit struct/interface embedding (base_types[], deferred /
	// edge-only-when-resolved); GO_SATISFIES = computed structural satisfaction
	// (interfaces[], via method-set matching). Deliberately NOT extends/implements.
	RelEmbeds    = "GO_EMBEDS"
	RelSatisfies = "GO_SATISFIES"

	// Imports — aggregated per target → :GoModule (in-project) or :GoExternal ghost.
	RelImports = "GO_IMPORTS"

	// Repository layer. Neutral (un-prefixed) edges are cross-language; the two
	// GO_ edges are this analyzer's own resolution claims.
	RelHasArtifact        = "HAS_ARTIFACT"
	RelDeclaresDependency = "DECLARES_DEPENDENCY" // _k=kind
	RelLocks              = "LOCKS"
	RelDefinesConfig      = "DEFINES_CONFIG"
	RelProvides           = "GO_PROVIDES"
	RelUnresolvedImport   = "GO_UNRESOLVED_IMPORT"

	// Reserved L3/L4 overlay — declared NOW, emits ZERO rows until the levels
	// land (CLAUDE.md L1). When L3/L4 arrive the projector fills these with no
	// schema-contract change. --emit neo4j is "full-depth = max implemented"
	// (L2 today); the graph grows additively.
	RelCFGNext  = "GO_CFG_NEXT" // _k=kind
	RelCDG      = "GO_CDG"
	RelDDG      = "GO_DDG" // _k=var,prov
	RelParamIn  = "GO_PARAM_IN"
	RelParamOut = "GO_PARAM_OUT"
	RelSummary  = "GO_SUMMARY"
)

// ─── the declared contract ──────────────────────────────────────────────────

// NodeSpec declares one node label: its MERGE label (labels[0] at projection
// time), its key property, and the typed property names it may carry. The
// conformance test (M3) holds the projector to the Props set.
type NodeSpec struct {
	// Label is the specific kind label (e.g. "GoType").
	Label string
	// MergeLabel is the label the uniqueness constraint and MERGE key on. It is
	// the shared SymbolLabel for the symbol family, else the label itself.
	MergeLabel string
	// Key is the merge key property (always "id" for can://-keyed nodes;
	// neutral nodes key on their own id/name).
	Key string
	// Props are every property name the projector may set on this node, beyond
	// the key. Used by the conformance test; order is not significant.
	Props []string
	// CanNode is true when the node is keyed by a can:// id and therefore
	// carries the :GoCanNode marker label and participates in prefix scoping.
	CanNode bool
}

// RelSpec declares one relationship type: its endpoints (by the labels it may
// connect) and the property names it may carry. Discriminant records the _k
// property used to keep legitimately-parallel edges from collapsing under MERGE.
type RelSpec struct {
	Type string
	// From / To are the label sets the endpoints may take. Used by the
	// conformance test to reject an edge wired to an undeclared endpoint.
	From []string
	To   []string
	// Props are the property names the edge may carry (excluding the _k
	// discriminant, which is Discriminant).
	Props []string
	// Discriminant names the _k key when parallel edges per (endpoint pair) are
	// legitimate (e.g. "kind" for GO_CFG_NEXT). Empty = plain endpoint-pair MERGE.
	Discriminant string
	// Deferred marks an edge whose target may be library/external code not
	// present in the graph: it is emitted via the RowBuilder's edge-only-when-
	// resolved path and kept at finish() only if the target was emitted.
	Deferred bool
	// Reserved marks the L3/L4 overlay edges: declared now, emits zero rows at
	// the current max level.
	Reserved bool
}

// Nodes is the full node-label contract, in projection (spine) order.
var Nodes = []NodeSpec{
	{
		Label: LabelApplication, MergeLabel: LabelApplication, Key: "id", CanNode: true,
		Props: []string{"schema_version", "language", "max_level", "analyzer_name", "analyzer_version"},
	},
	{
		Label: LabelModule, MergeLabel: LabelModule, Key: "id", CanNode: true,
		Props: []string{"package", "source", "content_hash", "span_json"},
	},
	{
		Label: LabelType, MergeLabel: SymbolLabel, Key: "id", CanNode: true,
		// kind ∈ struct|interface|alias|defined (property, decision T1).
		Props: []string{"kind", "signature", "span_json"},
	},
	{
		Label: LabelCallable, MergeLabel: SymbolLabel, Key: "id", CanNode: true,
		// source_file present only when it differs from the nesting module (D8);
		// error_channel is a homogeneous string array; is_* only on body nodes.
		Props: []string{"kind", "signature", "return_type", "error_channel", "source_file", "span_json", "metrics_json"},
	},
	{
		Label: LabelField, MergeLabel: LabelField, Key: "id", CanNode: true,
		Props: []string{"kind", "type", "span_json"},
	},
	{
		Label: LabelBodyNode, MergeLabel: LabelBodyNode, Key: "id", CanNode: true,
		// kind "call" at L1; is_goroutine/is_deferred pruned when false.
		Props: []string{"kind", "is_goroutine", "is_deferred", "span_json"},
	},
	{
		Label: LabelExternal, MergeLabel: SymbolLabel, Key: "id", CanNode: true,
		Props: []string{"name", "path"},
	},

	// Neutral layer. Keyed by own id (not a can:// id), so NO marker label and
	// outside prefix scoping — a sibling analyzer converges on the same node.
	{
		Label: LabelArtifact, MergeLabel: LabelArtifact, Key: "id", CanNode: false,
		Props: []string{"path", "format"},
	},
	{
		Label: LabelPackage, MergeLabel: LabelPackage, Key: "id", CanNode: false,
		Props: []string{"ecosystem", "name"},
	},
	{
		Label: LabelConfigKey, MergeLabel: LabelConfigKey, Key: "id", CanNode: false,
		Props: []string{"key", "value"},
	},
}

// Rels is the full relationship contract, grouped by family.
var Rels = []RelSpec{
	// Containment.
	{Type: RelHasModule, From: []string{LabelApplication}, To: []string{LabelModule}},
	{Type: RelDeclares, From: []string{LabelModule, LabelCallable}, To: []string{LabelType, LabelCallable}},
	{Type: RelHasMethod, From: []string{LabelType}, To: []string{LabelCallable}},
	{Type: RelHasField, From: []string{LabelType}, To: []string{LabelField}},
	{Type: RelHasBodyNode, From: []string{LabelCallable}, To: []string{LabelBodyNode}},

	// Call graph.
	{Type: RelCalls, From: []string{LabelCallable, LabelExternal}, To: []string{LabelCallable, LabelExternal}, Props: []string{"weight", "prov"}},
	{Type: RelResolvesTo, From: []string{LabelBodyNode}, To: []string{LabelCallable, LabelExternal}},

	// Inheritance.
	{Type: RelEmbeds, From: []string{LabelType}, To: []string{LabelType}, Deferred: true},
	{Type: RelSatisfies, From: []string{LabelType}, To: []string{LabelType}},

	// Imports.
	{Type: RelImports, From: []string{LabelModule}, To: []string{LabelModule, LabelExternal}, Props: []string{"alias", "positions_json"}},

	// Repository layer.
	{Type: RelHasArtifact, From: []string{LabelApplication}, To: []string{LabelArtifact}},
	{Type: RelDeclaresDependency, From: []string{LabelArtifact}, To: []string{LabelPackage}, Discriminant: "kind"},
	{Type: RelLocks, From: []string{LabelArtifact}, To: []string{LabelPackage}},
	{Type: RelDefinesConfig, From: []string{LabelArtifact}, To: []string{LabelConfigKey}},
	{Type: RelProvides, From: []string{LabelPackage}, To: []string{LabelExternal}},
	{Type: RelUnresolvedImport, From: []string{LabelApplication}, To: []string{LabelExternal}},

	// Reserved L3/L4 overlay (zero rows until the levels land).
	{Type: RelCFGNext, From: []string{LabelBodyNode}, To: []string{LabelBodyNode}, Discriminant: "kind", Reserved: true},
	{Type: RelCDG, From: []string{LabelBodyNode}, To: []string{LabelBodyNode}, Reserved: true},
	{Type: RelDDG, From: []string{LabelBodyNode}, To: []string{LabelBodyNode}, Props: []string{"var", "prov"}, Discriminant: "var", Reserved: true},
	{Type: RelParamIn, From: []string{LabelBodyNode}, To: []string{LabelBodyNode}, Reserved: true},
	{Type: RelParamOut, From: []string{LabelBodyNode}, To: []string{LabelBodyNode}, Reserved: true},
	{Type: RelSummary, From: []string{LabelBodyNode}, To: []string{LabelBodyNode}, Reserved: true},
}
