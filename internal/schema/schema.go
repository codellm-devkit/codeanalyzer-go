// Package schema defines the canonical data contract for codeanalyzer-go output.
//
// The root object is GoApplication{symbol_table, call_graph}. Every field uses
// snake_case JSON keys so the Python SDK's Pydantic models parse it without
// transformation. Design decisions are recorded in .claude/SCHEMA_DECISIONS.md.
package schema

// ─── Leaf models ─────────────────────────────────────────────────────────────

// GoImport represents a single import declaration in a Go source file.
type GoImport struct {
	Module    string `json:"module"`
	Alias     string `json:"alias,omitempty"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
}

// GoComment represents a comment (line, block, or doc comment).
type GoComment struct {
	Content     string `json:"content"`
	StartLine   int    `json:"start_line"`
	EndLine     int    `json:"end_line"`
	StartColumn int    `json:"start_column"`
	EndColumn   int    `json:"end_column"`
	IsDocComment bool  `json:"is_doc_comment"`
}

// GoParameter represents a single parameter of a callable.
type GoParameter struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	IsVariadic bool   `json:"is_variadic"`
	StartLine  int    `json:"start_line"`
	EndLine    int    `json:"end_line"`
}

// GoVariableDeclaration represents a variable declaration (var/short-assign).
type GoVariableDeclaration struct {
	Name        string `json:"name"`
	Type        string `json:"type,omitempty"`
	Initializer string `json:"initializer,omitempty"`
	Scope       string `json:"scope"` // "package" | "function"
	StartLine   int    `json:"start_line"`
	EndLine     int    `json:"end_line"`
	StartColumn int    `json:"start_column"`
	EndColumn   int    `json:"end_column"`
}

// GoField represents a struct field, including its struct tags.
type GoField struct {
	Name       string            `json:"name"`
	Type       string            `json:"type"`
	Comments   []GoComment       `json:"comments"`
	Tags       map[string]string `json:"tags"` // parsed struct tags, e.g. {"json": "name,omitempty"}
	IsExported bool              `json:"is_exported"`
	IsEmbedded bool              `json:"is_embedded"` // anonymous/embedded field
	StartLine  int               `json:"start_line"`
	EndLine    int               `json:"end_line"`
}

// GoSymbol represents a symbol accessed inside a callable.
type GoSymbol struct {
	Name          string `json:"name"`
	Scope         string `json:"scope"` // "local" | "package" | "external"
	Kind          string `json:"kind"`  // "variable" | "function" | "type" | "constant"
	Type          string `json:"type,omitempty"`
	QualifiedName string `json:"qualified_name,omitempty"`
	IsBuiltin     bool   `json:"is_builtin"`
	Lineno        int    `json:"lineno"`
	ColOffset     int    `json:"col_offset"`
}

// ─── Call site ────────────────────────────────────────────────────────────────

// GoCallsite represents a single call expression inside a callable.
// callee_signature is null when first recorded; the resolver backfills it
// during call-graph construction (never during symbol-table build).
type GoCallsite struct {
	MethodName      string   `json:"method_name"`
	ReceiverExpr    string   `json:"receiver_expr,omitempty"`
	ReceiverType    string   `json:"receiver_type,omitempty"`
	ArgumentTypes   []string `json:"argument_types"`
	ReturnType      string   `json:"return_type,omitempty"`
	CalleeSignature *string  `json:"callee_signature"` // null until resolved
	IsConstructorCall bool   `json:"is_constructor_call"`
	IsGoroutine     bool     `json:"is_goroutine"` // true when preceded by `go` keyword
	StartLine       int      `json:"start_line"`
	StartColumn     int      `json:"start_column"`
	EndLine         int      `json:"end_line"`
	EndColumn       int      `json:"end_column"`
}

// ─── Callable ─────────────────────────────────────────────────────────────────

// GoCallable represents a function, method, or function literal in Go.
// receiver_type / receiver_name are non-empty for methods; empty for functions.
type GoCallable struct {
	Name               string                           `json:"name"`
	Path               string                           `json:"path"`
	Signature          string                           `json:"signature"` // signatureOf() output — edge id
	Comments           []GoComment                      `json:"comments"`
	Parameters         []GoParameter                    `json:"parameters"`
	ReturnType         string                           `json:"return_type"`  // joined, e.g. "(int, error)"
	ReturnTypes        []string                         `json:"return_types"` // Go extension: individual return types
	Code               string                           `json:"code,omitempty"`
	StartLine          int                              `json:"start_line"`
	EndLine            int                              `json:"end_line"`
	CodeStartLine      int                              `json:"code_start_line"`
	AccessedSymbols    []GoSymbol                       `json:"accessed_symbols"`
	CallSites          []GoCallsite                     `json:"call_sites"`
	InnerCallables     map[string]GoCallable            `json:"inner_callables"`
	LocalVariables     []GoVariableDeclaration          `json:"local_variables"`
	CyclomaticComplexity int                            `json:"cyclomatic_complexity"`
	IsEntrypoint       bool                             `json:"is_entrypoint"`
	EntrypointFramework string                          `json:"entrypoint_framework,omitempty"`
	ReceiverType       string                           `json:"receiver_type,omitempty"` // e.g. "*MyStruct"
	ReceiverName       string                           `json:"receiver_name,omitempty"` // e.g. "r"
	IsExported         bool                             `json:"is_exported"`
}

// ─── Type (struct or interface) ───────────────────────────────────────────────

// GoType represents a named Go type — either a struct (is_interface=false) or
// an interface (is_interface=true). This unified model mirrors Go's native type
// system where both are types.Named with different Underlying() values.
//
// base_types carries: embedded struct types (for structs) and the method-set
// signatures of satisfied interfaces (for both) — the Go analog of base_classes.
type GoType struct {
	Name         string                  `json:"name"`
	Signature    string                  `json:"signature"` // signatureOf() output
	Comments     []GoComment             `json:"comments"`
	Code         string                  `json:"code,omitempty"`
	IsInterface  bool                    `json:"is_interface"`
	IsExported   bool                    `json:"is_exported"`
	Fields       []GoField               `json:"fields"`       // empty for interfaces
	Methods      map[string]GoCallable   `json:"methods"`      // sig → callable
	BaseTypes    []string                `json:"base_types"`   // embedded types + satisfied interface sigs
	InnerTypes   map[string]GoType       `json:"inner_types"`  // Go doesn't nest, but preserves spine
	StartLine    int                     `json:"start_line"`
	EndLine      int                     `json:"end_line"`
}

// ─── File (Module analog) ─────────────────────────────────────────────────────

// GoFile is the Module analog for Go: one compilation unit (source file).
// symbol_table is keyed by file path relative to the project root.
type GoFile struct {
	FilePath    string                           `json:"file_path"`
	PackageName string                           `json:"module_name"` // JSON key = module_name for spine compat
	Imports     []GoImport                       `json:"imports"`
	Comments    []GoComment                      `json:"comments"`
	Types       map[string]GoType                `json:"classes"`    // JSON key = classes for spine compat
	Functions   map[string]GoCallable            `json:"functions"`
	Variables   []GoVariableDeclaration          `json:"variables"`
	// Caching metadata
	ContentHash  *string  `json:"content_hash"`
	LastModified *float64 `json:"last_modified"`
	FileSize     *int64   `json:"file_size"`
}

// ─── Call graph edge ──────────────────────────────────────────────────────────

// GoCallEdge is an identity-only call-graph edge. source and target are
// GoCallable.signature strings that must exist in the symbol table.
type GoCallEdge struct {
	Source     string            `json:"source"`
	Target     string            `json:"target"`
	Type       string            `json:"type"`   // always "CALL_DEP"
	Weight     int               `json:"weight"` // accumulated when merging backends
	Provenance []string          `json:"provenance"` // e.g. ["go/types"], ["go/types","codeql"]
	Tags       map[string]string `json:"tags"`
}

// ─── Root object ──────────────────────────────────────────────────────────────

// GoApplication is the root of analysis.json. The SDK facade deserializes this.
type GoApplication struct {
	SymbolTable map[string]GoFile `json:"symbol_table"` // file_path → GoFile
	CallGraph   []GoCallEdge      `json:"call_graph"`   // identity-only edges
	Entrypoints map[string][]GoEntrypoint `json:"entrypoints"` // optional, default {}
}

// GoEntrypoint marks a callable as a framework entry point.
type GoEntrypoint struct {
	Signature       string            `json:"signature"`
	Framework       string            `json:"framework"`
	DetectionSource string            `json:"detection_source"`
	Tags            map[string]string `json:"tags"`
}
