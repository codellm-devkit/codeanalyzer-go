// Package v2 defines the canonical CLDK schema (v2) types that codeanalyzer-go
// emits. It is a faithful transcript of the keystone
// (designing-cldk-changes/references/canonical-schema.md) and the Go-specific
// leaf additions recorded in the repo-root CLAUDE.md § Schema decisions.
//
// The model is ONE additive containment tree
// (application → module → type → callable → body) with typed edge overlays
// laid over it (a Code Property Graph). This package declares the L1 tree plus
// the L2 refinement/edge slots; L3/L4 fields (cfg/cdg/ddg/param_*) are not
// declared here — they arrive in their own train and reuse these ids/spans
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

// SchemaVersion is the canonical schema major this package targets.
const SchemaVersion = "2.0.0"

// Language is the analyzer's language tag in the manifest and `can://<lang>/…`.
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
// rather than sniffing for keys.
type Analysis struct {
	SchemaVersion string      `json:"schema_version"`
	Language      string      `json:"language"`
	MaxLevel      int         `json:"max_level"`
	Analyzer      AnalyzerTag `json:"analyzer"`
	Application   Application `json:"application"`
}

// AnalyzerTag identifies the producing analyzer and version.
type AnalyzerTag struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Application is the root node of the containment tree.
type Application struct {
	// ID is `can://<lang>/<app>` — the app segment disambiguates apps in one
	// language. <app> is the --app-name value (default: input dir base name).
	ID   string `json:"id"`
	Kind string `json:"kind"` // always "application"
	// SymbolTable is the L1 named map keyed by relative file path.
	SymbolTable map[string]Module `json:"symbol_table"`
	// CallGraph holds the L2 cross-function edges (callable → callable). Empty
	// at L1.
	CallGraph []Edge `json:"call_graph"`
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
// (line:col for real nodes). At L1 the only kind emitted is "call"; L3 adds the
// remaining statement kinds.
type BodyNode struct {
	Kind string `json:"kind"` // "call" at L1
	Span Span   `json:"span"`
	// Callee is the sanctioned null→id refinement slot on a call node: null at
	// L1, backfilled to a callable id at L2. A pointer with NO omitempty so it
	// always serializes — as JSON null at L1 (the one place null is allowed),
	// as an id once backfilled. The keystone requires the field to be present.
	Callee *string `json:"callee"`
	// IsGoroutine is true when the call is preceded by `go`.
	IsGoroutine bool `json:"is_goroutine,omitempty"`
	// IsDeferred is true when the call is preceded by `defer` (net-new vs v1).
	IsDeferred bool `json:"is_deferred,omitempty"`
}

// ─── edges ─────────────────────────────────────────────────────────────────────

// Edge is a typed edge overlay record. The list it lives in IS its type (there
// is no `type` field). src/dst are node ids; no dangling endpoints.
type Edge struct {
	Src string `json:"src"`
	Dst string `json:"dst"`
	// Prov is open-vocabulary provenance, e.g. ["go/types"], ["go/types","codeql"].
	Prov []string `json:"prov,omitempty"`
	// Weight accumulates when merging edges from multiple backends.
	Weight int `json:"weight,omitempty"`
}
