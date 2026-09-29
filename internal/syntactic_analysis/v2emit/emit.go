package v2emit

import (
	"os"
	"path/filepath"
	"sort"

	"github.com/codellm-devkit/codeanalyzer-go/internal/schema"
	v2 "github.com/codellm-devkit/codeanalyzer-go/internal/schema/v2"
)

// Emit transforms the v1 GoApplication into the v2 Analysis payload. appName is
// the application anchor (--app-name); projectDir is the absolute input root,
// used to read each module's source for span slicing. maxLevel records how
// deeply the tree was populated (1 = symbol table, 2 = + call_graph);
// analyzerVersion is stamped into the manifest's analyzer{} tag.
//
// This is a pure re-serialization: every fact comes from the v1 model, except
// span byte offsets, which are derived from source via lineIndex.
func Emit(app *schema.GoApplication, appName, projectDir string, maxLevel int, analyzerVersion string) *v2.Analysis {
	appID := v2.AppID(appName)

	// The signature→can:// id index translates v1 identity (signature strings)
	// into v2 node ids. It is what the L2 refinement uses to backfill body
	// callees and to map call_graph edge endpoints onto real callable ids.
	idx := buildSigIndex(appID, app.SymbolTable)

	out := &v2.Analysis{
		SchemaVersion: v2.SchemaVersion,
		Language:      v2.Language,
		MaxLevel:      maxLevel,
		Analyzer:      v2.AnalyzerTag{Name: "codeanalyzer-go", Version: analyzerVersion},
		Application: v2.Application{
			ID:          appID,
			Kind:        "application",
			SymbolTable: make(map[string]v2.Module, len(app.SymbolTable)),
			CallGraph:   emitCallGraph(app.CallGraph, idx),
		},
	}

	// Deterministic module order does not affect the JSON map, but iterating
	// sorted keeps any incidental ordering (and logs) stable across runs.
	for _, relPath := range sortedFileKeys(app.SymbolTable) {
		file := app.SymbolTable[relPath]
		out.Application.SymbolTable[relPath] = emitModule(appID, projectDir, relPath, file, idx)
	}
	return out
}

// emitModule builds a v2 module from a v1 GoFile, reading the file's source so
// spans can slice off it. A file that cannot be read still emits its structure
// (with empty source and zeroed byte spans) rather than dropping the module.
func emitModule(appID, projectDir, relPath string, file schema.GoFile, idx sigIndex) v2.Module {
	source := readSource(projectDir, relPath)
	li := newLineIndex(source)
	modID := v2.ModuleID(appID, relPath)

	mod := v2.Module{
		ID:        modID,
		Kind:      "module",
		Span:      wholeFileSpan(li, source),
		Package:   file.PackageName,
		Source:    source,
		Imports:   emitImports(li, file.Imports),
		Types:     make(map[string]v2.Type, len(file.Types)),
		Functions: make(map[string]v2.Callable, len(file.Functions)),
	}
	if file.ContentHash != nil {
		mod.ContentHash = *file.ContentHash
	}

	for name, t := range file.Types {
		mod.Types[name] = emitType(modID, li, t, idx)
	}
	for sig, fn := range file.Functions {
		mod.Functions[sig] = emitCallable(modID, li, fn, idx)
	}
	return mod
}

// emitType maps a v1 GoType to a v2 type node, collapsing is_interface into the
// kind and splitting embedding (base_types) from computed satisfaction. Methods
// resolve into the type's callables{}.
func emitType(modID string, li *lineIndex, t schema.GoType, idx sigIndex) v2.Type {
	typeID := v2.TypeID(modID, t.Signature)
	out := v2.Type{
		ID:        typeID,
		Kind:      typeKind(t),
		Span:      lineSpan(li, t.StartLine, t.EndLine),
		BaseTypes: nonEmpty(t.BaseTypes),
		Callables: make(map[string]v2.Callable, len(t.Methods)),
	}
	if len(t.Fields) > 0 {
		out.Fields = make(map[string]v2.Field, len(t.Fields))
		for _, f := range t.Fields {
			out.Fields[f.Name] = v2.Field{
				ID:   typeID + "/" + f.Name,
				Kind: "field",
				Type: f.Type,
				Span: lineSpan(li, f.StartLine, f.EndLine),
			}
		}
	}
	for sig, m := range t.Methods {
		out.Callables[sig] = emitCallable(typeID, li, m, idx)
	}
	return out
}

// emitCallable maps a v1 GoCallable to a v2 callable node, including its body
// (call nodes) and nested closures.
func emitCallable(parentID string, li *lineIndex, c schema.GoCallable, idx sigIndex) v2.Callable {
	callID := v2.CallableID(parentID, c.Signature)
	out := v2.Callable{
		ID:         callID,
		Kind:       callableKind(c),
		Span:       lineSpan(li, c.StartLine, c.EndLine),
		Signature:  c.Signature,
		Parameters: emitParams(li, c.Parameters),
		ReturnType: c.ReturnType,
		Body:       emitBody(callID, li, c.CallSites, idx),
	}
	if ch := errorChannel(c.ReturnTypes); len(ch) > 0 {
		out.ErrorChannel = ch
	}
	if c.CyclomaticComplexity > 0 {
		out.Metrics = map[string]int{"cyclomatic": c.CyclomaticComplexity}
	}
	if len(c.InnerCallables) > 0 {
		out.Callables = make(map[string]v2.Callable, len(c.InnerCallables))
		for sig, inner := range c.InnerCallables {
			out.Callables[sig] = emitCallable(callID, li, inner, idx)
		}
	}
	return out
}

// emitBody builds the body map: one `call` node per call site, keyed by its
// line:col local id, carrying the sanctioned callee null->id refinement slot.
//
// callee resolution:
//   - L1: the v1 site has no backfilled signature, so callee stays null.
//   - L2: the resolver backfilled the site's callee signature. When that
//     signature names an in-tree callable, callee becomes its can:// id; when
//     it names an external/stdlib callee (no in-tree node), callee stays null —
//     the honest-unresolved fallback the L2 gate expects.
func emitBody(callableID string, li *lineIndex, sites []schema.GoCallsite, idx sigIndex) map[string]v2.BodyNode {
	body := make(map[string]v2.BodyNode, len(sites))
	for _, cs := range sites {
		local := v2.LocalID(cs.StartLine, cs.StartColumn)
		body[local] = v2.BodyNode{
			Kind:        "call",
			Span:        colSpan(li, cs.StartLine, cs.StartColumn, cs.EndLine, cs.EndColumn),
			Callee:      calleeID(cs.CalleeSignature, idx),
			IsGoroutine: cs.IsGoroutine,
		}
	}
	return body
}

// calleeID translates a v1 backfilled callee signature into the v2 can:// id
// refinement slot: nil (JSON null) when unresolved or external, a pointer to the
// in-tree callable id when resolved. Never returns the raw v1 signature — a
// call node's callee is always a node id or null.
func calleeID(sig *string, idx sigIndex) *string {
	if sig == nil {
		return nil
	}
	id, ok := idx.idFor(*sig)
	if !ok {
		return nil
	}
	return &id
}

func emitParams(li *lineIndex, params []schema.GoParameter) []v2.Parameter {
	out := make([]v2.Parameter, 0, len(params))
	for _, p := range params {
		out = append(out, v2.Parameter{
			Name:       p.Name,
			Type:       p.Type,
			Span:       lineSpan(li, p.StartLine, p.EndLine),
			IsVariadic: p.IsVariadic,
		})
	}
	return out
}

func emitImports(li *lineIndex, imports []schema.GoImport) []v2.Import {
	out := make([]v2.Import, 0, len(imports))
	for _, imp := range imports {
		out = append(out, v2.Import{
			Name:  packageBase(imp.Module),
			Path:  imp.Module,
			Alias: imp.Alias,
			Span:  lineSpan(li, imp.StartLine, imp.EndLine),
		})
	}
	return out
}

// emitCallGraph maps v1 identity-only edges to the v2 {src,dst,prov,weight}
// shape: the source/target/provenance rename, PLUS the endpoint translation from
// v1 signatures to can:// callable ids. An edge whose src or dst does not resolve
// to an in-tree node is dropped rather than emitted with a dangling endpoint —
// the L2 gate requires every endpoint to be a real callable id. (The v1 resolver
// only emits edges to in-project targets, so this drop is defensive.)
func emitCallGraph(edges []schema.GoCallEdge, idx sigIndex) []v2.Edge {
	out := make([]v2.Edge, 0, len(edges))
	for _, e := range edges {
		src, srcOK := idx.idFor(e.Source)
		dst, dstOK := idx.idFor(e.Target)
		if !srcOK || !dstOK {
			continue
		}
		out = append(out, v2.Edge{
			Src:    src,
			Dst:    dst,
			Prov:   nonEmpty(e.Provenance),
			Weight: e.Weight,
		})
	}
	return out
}

// ─── kind mapping ────────────────────────────────────────────────────────────

// typeKind collapses the v1 is_interface boolean into the v2 kind vocabulary.
// v1 only distinguishes struct vs interface; alias/defined await richer v1
// facts, so a non-interface type maps to struct for now.
func typeKind(t schema.GoType) string {
	if t.IsInterface {
		return "interface"
	}
	return "struct"
}

// callableKind classifies a callable: a non-empty receiver type marks a method.
func callableKind(c schema.GoCallable) string {
	if c.ReceiverType != "" {
		return "method"
	}
	return "function"
}

// errorChannel extracts error-typed returns into the generalized error surface.
func errorChannel(returnTypes []string) []string {
	var ch []string
	for _, rt := range returnTypes {
		if rt == "error" {
			ch = append(ch, rt)
		}
	}
	return ch
}

// ─── span construction ─────────────────────────────────────────────────────────

// wholeFileSpan is the module node's span: the entire file.
func wholeFileSpan(li *lineIndex, source string) v2.Span {
	lastLine := len(li.lineStart)
	if lastLine == 0 {
		lastLine = 1
	}
	return v2.NewSpan(1, 1, lastLine, li.lineLen(lastLine)+1, 0, len(source))
}

// lineSpan builds a span from a line-only v1 position: start at column 1, end at
// the last line's end. Byte offsets bound the whole line range.
func lineSpan(li *lineIndex, startLine, endLine int) v2.Span {
	if endLine < startLine {
		endLine = startLine
	}
	endCol := li.lineLen(endLine) + 1
	return v2.NewSpan(
		startLine, 1, endLine, endCol,
		li.offset(startLine, 1), li.lineEndOffset(endLine),
	)
}

// colSpan builds a span from a v1 position that carries columns (call sites).
func colSpan(li *lineIndex, startLine, startCol, endLine, endCol int) v2.Span {
	return v2.NewSpan(
		startLine, startCol, endLine, endCol,
		li.offset(startLine, startCol), li.offset(endLine, endCol),
	)
}

// ─── helpers ─────────────────────────────────────────────────────────────────

func readSource(projectDir, relPath string) string {
	data, err := os.ReadFile(filepath.Join(projectDir, relPath))
	if err != nil {
		return ""
	}
	return string(data)
}

func sortedFileKeys(m map[string]schema.GoFile) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// packageBase returns the last path segment of an import path as the package
// name, e.g. "hash/fnv" -> "fnv".
func packageBase(importPath string) string {
	return filepath.Base(importPath)
}

// nonEmpty returns s unchanged, or nil if empty, so omitempty drops the field
// rather than emitting an empty array (absence = no fact).
func nonEmpty(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return s
}
