// Package v2 defines the canonical CLDK schema (v2) types that codeanalyzer-go
// emits. It is a faithful transcript of the keystone
// (designing-cldk-changes/references/canonical-schema.md) and the Go-specific
// leaf additions recorded in the repo-root CLAUDE.md § Schema decisions.
//
// The model is ONE additive containment tree
// (application → module → type → callable → body) with typed edge overlays
// laid over it (a Code Property Graph). This package declares the L1 tree, the
// L2 refinement/edge slots, the L3 body completion (statement nodes, body
// node id/parent, the callable's cfg/cdg/ddg) and the L4 fields (the envelope's
// degraded marker, body node `of`, a callable's summary, the application's
// param_in/param_out, an external symbol's receiver and body). The L4 fields
// are all omitempty and absent below -a 4; they reuse the L1-L3 ids/spans
// unchanged.
//
// Conventions from the keystone, held here:
//   - snake_case JSON keys everywhere, so one set of SDK models parses every
//     analyzer.
//   - a fact is present or absent; there is no null, EXCEPT the one sanctioned
//     `callee: null` refinement slot on a call node (null at L1, backfilled at
//     L2). Absence is the "no fact" encoding — `omitempty` on optional fields.
//   - open-vocabulary fields (prov, tags) are plain strings.
package v2

import "encoding/json"

// SchemaVersion is the canonical schema major this package targets.
const SchemaVersion = "2.0.0"

// Language is the analyzer's language tag in the manifest and `can://<app>/<lang>/…`.
const Language = "go"

// ─── Positioning ───────────────────────────────────────────────────────────

// Span is the one universal node attribute: where in source the node lives.
// line:col addresses and displays; bytes slices module.source in O(1). Byte
// offsets are UTF-8 offsets into module.source (Go source is natively UTF-8).
type Span struct {
	// Start is [line, col] of the first byte (1-based line, 1-based col).
	Start [2]int `json:"start"`
	// End is [line, col] one past the last byte.
	End [2]int `json:"end"`
	// Bytes is [from, to) UTF-8 byte offsets into module.source.
	Bytes [2]int `json:"bytes"`
}

// ─── Root envelope ───────────────────────────────────────────────────────────

// Analysis is the top-level payload written to analysis.json / stdout. Its
// siblings of `application` carry the manifest; consumers read `max_level`
// rather than sniffing for keys. k_limit (the field-path depth bound of the
// L3 and L4 access paths) is present only at max_level >= 3.
type Analysis struct {
	SchemaVersion string      `json:"schema_version"`
	Language      string      `json:"language"`
	MaxLevel      int         `json:"max_level"`
	KLimit        int         `json:"k_limit,omitempty"`
	Analyzer      AnalyzerTag `json:"analyzer"`
	Application   Application `json:"application"`
	// Degraded records the L4 degradation markers (spec §4.15); absent when
	// the run degraded nothing and below -a 4.
	Degraded []Degraded `json:"degraded,omitempty"`
}

// Degraded is one degradation marker: an L4 step that fell back to a coarser
// result. Code is an open vocabulary of plain strings (spec §4.15); Detail,
// Packages and Callables say what was affected.
type Degraded struct {
	Code      string   `json:"code"`
	Detail    string   `json:"detail,omitempty"`
	Packages  []string `json:"packages,omitempty"`
	Callables []string `json:"callables,omitempty"`
}

// AnalyzerTag identifies the producing analyzer and version.
type AnalyzerTag struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Application is the root node of the containment tree.
type Application struct {
	// ID is `can://<app>` — the app segment is outermost, so a polyglot
	// app shares one prefix across languages. <app> is the --app-name value (default: input dir base name).
	ID   string `json:"id"`
	Kind string `json:"kind"` // always "application"
	// SymbolTable is the L1 named map keyed by relative file path.
	SymbolTable map[string]Module `json:"symbol_table"`
	// CallGraph holds the L2 cross-function edges (callable → callable). Empty
	// at L1.
	CallGraph []Edge `json:"call_graph"`
	// ExternalSymbols holds the out-of-project (library) callables the call graph
	// reaches, keyed by their `can://<app>/@external/<import-path>/<sig>` id.
	// Absent when the analysis saw no library call.
	ExternalSymbols map[string]ExternalSymbol `json:"external_symbols,omitempty"`
	// ParamIn and ParamOut are the L4 interprocedural parameter edges between
	// global body-node ids. Absent below -a 4.
	ParamIn  []ParamEdge `json:"param_in,omitempty"`
	ParamOut []ParamEdge `json:"param_out,omitempty"`
}

// ParamEdge is an L4 interprocedural parameter edge (call-site actual →
// callee formal for param_in, callee formal/return → call-site result for
// param_out) on the access path Var.
type ParamEdge struct {
	Src string `json:"src"`
	Dst string `json:"dst"`
	Var string `json:"var"`
}

// ExternalSymbol is a library callable that call-graph edges and call nodes
// target but whose body is not in the project. It is a leaf node: identity and
// signature only.
type ExternalSymbol struct {
	ID   string `json:"id"`
	Kind string `json:"kind"` // always "external"
	Name string `json:"name"`
	// Module is the package import path the symbol lives in.
	Module string `json:"module"`
	// Signature is the id's last segment: [Type.]Name, no import-path prefix.
	Signature  string              `json:"signature"`
	Parameters []ExternalParameter `json:"parameters"`
	ReturnType string              `json:"return_type,omitempty"`
	// Receiver is the method receiver (absent on a plain function): the name
	// is the receiver variable, or _recv when unnamed or `_`; the type is the
	// origin's receiver type string.
	Receiver *ExternalParameter `json:"receiver,omitempty"`
	// Body holds the L4 stand-in body nodes of a library callee (spec §6).
	Body map[string]BodyNode `json:"body,omitempty"`
}

// ExternalParameter is a library callee's formal parameter. It has no span: the
// declaration is not in the project, so there is no location to slice.
type ExternalParameter struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	IsVariadic bool   `json:"is_variadic,omitempty"`
}

// ─── module (per-file compilation unit) ───────────────────────────────────────

// Module is one Go source file: the L1 per-file container.
type Module struct {
	ID   string `json:"id"`
	Kind string `json:"kind"` // always "module"
	Span Span   `json:"span"`
	// Package is the Go package the file belongs to.
	Package string `json:"package"`
	// Source is the WHOLE file's text, stored once. Every node's text slices
	// from this via its span.bytes; there is no per-node `code`.
	Source string `json:"source"`
	// Imports are the file's import declarations.
	Imports []Import `json:"imports"`
	// Types is the named map of type declarations in the file.
	Types map[string]Type `json:"types"`
	// Functions is the named map of module-level callables (keyed by signature).
	Functions map[string]Callable `json:"functions"`
	// ContentHash supports incremental caching; it is not identity.
	ContentHash string `json:"content_hash,omitempty"`
}

// Import is a single import declaration.
type Import struct {
	Name  string `json:"name"`            // imported package name
	Path  string `json:"path"`            // import path, e.g. "hash/fnv"
	Alias string `json:"alias,omitempty"` // present only when aliased
	Span  Span   `json:"span"`
}

// ─── type ─────────────────────────────────────────────────────────────────────

// Type is a named Go type. Kind is the specific type-declaration shape, not a
// pile of is_* booleans.
type Type struct {
	ID   string `json:"id"`
	Kind string `json:"kind"` // struct | interface | alias | defined
	Span Span   `json:"span"`
	// BaseTypes are embedded type ids (struct/interface embedding — the explicit
	// spine).
	BaseTypes []string `json:"base_types,omitempty"`
	// Interfaces are ids of interfaces this type is computed to satisfy via
	// method-set matching (Go's structural implementation).
	Interfaces []string `json:"interfaces,omitempty"`
	// Callables are the type's methods, resolved to their receiver type
	// regardless of source location. Keyed by signature.
	Callables map[string]Callable `json:"callables"`
	// Fields are the struct fields, keyed by name.
	Fields map[string]Field `json:"fields,omitempty"`
}

// Field is a struct field.
type Field struct {
	ID   string `json:"id"`
	Kind string `json:"kind"` // always "field"
	Type string `json:"type"`
	Span Span   `json:"span"`
}

// ─── callable ─────────────────────────────────────────────────────────────────

// Callable is a function, method, or closure. span.bytes → get_method_body =
// module.source[bytes].
type Callable struct {
	ID   string `json:"id"`
	Kind string `json:"kind"` // function | method | lambda
	Span Span   `json:"span"`
	// Signature is the human-readable last path segment of ID (one signatureOf()).
	Signature string `json:"signature"`
	// SourceFile names the callable's declaring file (a symbol_table key) when it
	// differs from the module this callable is nested under — the Go case where a
	// method is declared in a different file than its receiver type, and every
	// closure (nested closures included) inside such a method. When set,
	// Span.Bytes index symbol_table[SourceFile].source, NOT the nesting module's
	// source; text = symbol_table[SourceFile].source[Span.Bytes]. Absent (the
	// common case) means the declaring file is the nesting module. See CLAUDE.md
	// § Schema decisions → callable.source_file and docs/design/specs/v2-l1-emission.md.
	SourceFile string `json:"source_file,omitempty"`
	// Parameters are the ordered formal parameters.
	Parameters []Parameter `json:"parameters"`
	// ReturnType is the joined return type, e.g. "(int, error)".
	ReturnType string `json:"return_type,omitempty"`
	// ErrorChannel is the generalized error surface: populated from error-typed
	// returns (those returns remain in ReturnType too).
	ErrorChannel []string `json:"error_channel,omitempty"`
	// Metrics is an extensible metrics map (e.g. {"cyclomatic": 3}).
	Metrics map[string]int `json:"metrics,omitempty"`
	// Body holds call nodes at L1 (keyed by local id), completing at L3.
	Body map[string]BodyNode `json:"body"`
	// Cfg, Cdg and Ddg are the L3 intra-callable edge lists; endpoints are
	// local body keys. Absent below L3 and for a callable with no body.
	Cfg []CfgEdge `json:"cfg,omitempty"`
	Cdg []CdgEdge `json:"cdg,omitempty"`
	Ddg []DdgEdge `json:"ddg,omitempty"`
	// Summary is the L4 summary edge list (formal-in → formal-out through the
	// callable); endpoints are local body keys. Absent below -a 4.
	Summary []SummaryEdge `json:"summary,omitempty"`
	// Callables are nested closures / function literals, each with its own
	// can:// id (replaces v1 InnerCallables).
	Callables map[string]Callable `json:"callables,omitempty"`
}

// Parameter is a single formal parameter.
type Parameter struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	Span       Span   `json:"span"`
	IsVariadic bool   `json:"is_variadic,omitempty"`
}

// ─── body nodes ────────────────────────────────────────────────────────────────

// BodyNode is a node inside a callable's body, keyed by its local id
// (line:col for real nodes, "@entry"/"@exit" for the synthetic ones). At L1
// the only kind emitted is "call"; L3 adds statement | branch | loop | switch
// | return | entry | exit.
type BodyNode struct {
	Kind string `json:"kind"` // "call" at L1
	// ID is the node's global ordinal id, v2.GlobalOrdinal(callable id, key).
	// Emitted at every level (from L1, R15 — Python parity), on every body
	// node.
	ID string `json:"id,omitempty"`
	// Span is the node's source range. Absent (zero) only on the synthetic
	// entry/exit nodes, which have no source.
	Span Span `json:"span,omitzero"`
	// Parent is, at L3+, the local key of the statement a satellite call
	// node is nested in (the sanctioned parent refinement). Absent on CFG
	// nodes.
	Parent string `json:"parent,omitempty"`
	// Callee is the sanctioned null→id refinement slot on a call node: null at
	// L1, backfilled to a callable id at L2. A pointer with NO omitempty so it
	// always serializes — as JSON null at L1 (the one place null is allowed),
	// as an id once backfilled. The keystone requires the field to be present.
	Callee *string `json:"callee"`
	// IsGoroutine is true when the call is preceded by `go`.
	IsGoroutine bool `json:"is_goroutine,omitempty"`
	// IsDeferred is true when the call is preceded by `defer` (net-new vs v1).
	IsDeferred bool `json:"is_deferred,omitempty"`
	// Of names the value an L4 vertex carries (<return>, <return>:<i>, a
	// parameter name, <capture>:<name>, <global>:<pkg>::<Name>). Absent below
	// -a 4 and on L1-L3 nodes.
	Of string `json:"of,omitempty"`
}

// SummaryEdge is an L4 summary edge between two local body keys of one callable.
type SummaryEdge struct {
	Src string `json:"src"`
	Dst string `json:"dst"`
}

// MarshalJSON writes the callee key on call nodes only: it is the call
// node's refinement slot (always present there, null until resolved), and a
// statement / control / synthetic node has no callee fact to carry.
func (n BodyNode) MarshalJSON() ([]byte, error) {
	type plain BodyNode
	if n.Kind == "call" {
		return json.Marshal(plain(n))
	}
	return json.Marshal(struct {
		plain
		Callee *string `json:"callee,omitempty"`
	}{plain: plain(n)})
}

// ─── edges ─────────────────────────────────────────────────────────────────────

// CfgEdge is an L3 control-flow edge between two local body keys. Kind is a
// shared CFG edge kind or the Go add "defer_resume".
type CfgEdge struct {
	Src  string `json:"src"`
	Dst  string `json:"dst"`
	Kind string `json:"kind"`
}

// CdgEdge is an L3 control-dependence edge between two local body keys.
type CdgEdge struct {
	Src string `json:"src"`
	Dst string `json:"dst"`
}

// DdgEdge is a data-dependence edge between two local body keys on the
// k-limited access path Var. Prov is ["ssa"] at L3 (syntactic reaching
// definitions; the keystone's token, shared with the siblings).
type DdgEdge struct {
	Src  string   `json:"src"`
	Dst  string   `json:"dst"`
	Var  string   `json:"var"`
	Prov []string `json:"prov"`
}

// Edge is a typed edge overlay record. The list it lives in IS its type (there
// is no `type` field). src/dst are node ids; no dangling endpoints.
type Edge struct {
	Src string `json:"src"`
	Dst string `json:"dst"`
	// Prov is open-vocabulary provenance, e.g. ["go/types"].
	Prov []string `json:"prov,omitempty"`
	// Weight accumulates when merging edges from multiple backends.
	Weight int `json:"weight,omitempty"`
}
