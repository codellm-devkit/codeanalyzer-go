package v2emit

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

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

	// One lineIndex per source file, read once and shared. A method whose
	// receiver type lives in a different file is nested under that type's module
	// but must have its span computed against ITS OWN file (CLAUDE.md § Schema
	// decisions → callable.source_file); the cache lets emitCallable reach any
	// file's index without re-reading it per callable.
	srcs := newSourceCache(projectDir)

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
		out.Application.SymbolTable[relPath] = emitModule(appID, relPath, file, idx, srcs)
	}
	return out
}

// emitModule builds a v2 module from a v1 GoFile, reading the file's source so
// spans can slice off it. A file that cannot be read still emits its structure
// (with empty source and zeroed byte spans) rather than dropping the module.
//
// modRel is this module's own file path (a symbol_table key). It is threaded to
// callables so a method declared in another file can be detected and given a
// span against its own source plus a source_file pointer.
func emitModule(appID, modRel string, file schema.GoFile, idx sigIndex, srcs *sourceCache) v2.Module {
	li := srcs.index(modRel)
	source := li.source
	modID := v2.ModuleID(appID, modRel)

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
		mod.Types[name] = emitType(modID, modRel, t, idx, srcs)
	}
	for sig, fn := range file.Functions {
		mod.Functions[sig] = emitCallable(modID, modRel, li, fn, idx, srcs)
	}
	return mod
}

// emitType maps a v1 GoType to a v2 type node, collapsing is_interface into the
// kind and splitting embedding (base_types) from computed satisfaction. Methods
// resolve into the type's callables{}. modRel is the module the type lives in;
// its methods may be declared in other files (handled in emitCallable).
func emitType(modID, modRel string, t schema.GoType, idx sigIndex, srcs *sourceCache) v2.Type {
	li := srcs.index(modRel)
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
		out.Callables[sig] = emitCallable(typeID, modRel, li, m, idx, srcs)
	}
	return out
}

// emitCallable maps a v1 GoCallable to a v2 callable node, including its body
// (call nodes) and nested closures.
//
// modRel and modLI are the nesting module's path and lineIndex. When the
// callable's declaring file (c.Path) differs — a method whose receiver type is
// in another file — its span is computed against its OWN file's lineIndex and
// source_file records that file, per CLAUDE.md § Schema decisions. Nested
// closures share their enclosing callable's file (Go has no cross-file
// closures), so they carry the resolved file down unchanged.
func emitCallable(parentID, modRel string, modLI *lineIndex, c schema.GoCallable, idx sigIndex, srcs *sourceCache) v2.Callable {
	callID := v2.CallableID(parentID, c.Signature)

	// Resolve the file this callable's positions are relative to.
	li := modLI
	sourceFile := ""
	if c.Path != "" && c.Path != modRel {
		li = srcs.index(c.Path)
		sourceFile = c.Path
	}

	out := v2.Callable{
		ID:         callID,
		Kind:       callableKind(c),
		Span:       lineSpan(li, c.StartLine, c.EndLine),
		Signature:  c.Signature,
		SourceFile: sourceFile,
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
		// Closures live in the same file as their enclosing callable, so the
		// resolved file (modRel-or-c.Path) becomes their nesting file.
		enclRel := modRel
		if sourceFile != "" {
			enclRel = sourceFile
		}
		for sig, inner := range c.InnerCallables {
			out.Callables[sig] = emitCallable(callID, enclRel, li, inner, idx, srcs)
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

	// Impose a total order on the edges so the output is byte-identical across
	// runs and across -j values (release-gates.md determinism gate). The upstream
	// edges slice is assembled by ranging a Go map (random iteration order), so
	// without this sort two runs of the same input emit the call_graph in
	// different orders. Sort on the full tuple — (src, dst) is the identity but
	// prov/weight tie-break so merged edges from multiple backends also order
	// stably.
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Src != b.Src {
			return a.Src < b.Src
		}
		if a.Dst != b.Dst {
			return a.Dst < b.Dst
		}
		ap, bp := strings.Join(a.Prov, ","), strings.Join(b.Prov, ",")
		if ap != bp {
			return ap < bp
		}
		return a.Weight < b.Weight
	})
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

// sourceCache reads each source file once and memoizes its lineIndex, keyed by
// path relative to projectDir. The emitter needs a file's index in two places —
// when emitting the module itself, and when a method declared in that file is
// nested under a type in a DIFFERENT module — so the cache avoids re-reading a
// file per cross-file method. A file that cannot be read caches an empty index
// (so spans degrade to zero rather than re-attempting the read).
type sourceCache struct {
	projectDir string
	byPath     map[string]*lineIndex
}

func newSourceCache(projectDir string) *sourceCache {
	return &sourceCache{projectDir: projectDir, byPath: make(map[string]*lineIndex)}
}

// index returns the lineIndex for relPath, reading and indexing the file on
// first use. The returned index carries the file's source (li.source).
func (c *sourceCache) index(relPath string) *lineIndex {
	if li, ok := c.byPath[relPath]; ok {
		return li
	}
	li := newLineIndex(readSource(c.projectDir, relPath))
	c.byPath[relPath] = li
	return li
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
