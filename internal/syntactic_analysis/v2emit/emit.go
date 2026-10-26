package v2emit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/codellm-devkit/codeanalyzer-go/internal/dataflow"
	"github.com/codellm-devkit/codeanalyzer-go/internal/schema"
	v2 "github.com/codellm-devkit/codeanalyzer-go/internal/schema/v2"
)

// Emit transforms the v1 GoApplication into the v2 Analysis payload. appName is
// the application anchor (--app-name); projectDir is the absolute input root,
// used to read each module's source for span slicing. maxLevel records how
// deeply the tree was populated (1 = symbol table, 2 = + call_graph, 3 = +
// cfg/cdg/ddg); analyzerVersion is stamped into the manifest's analyzer{} tag.
//
// kLimit is the L3 access-path bound (--graph-field-depth), written as the
// envelope's k_limit at maxLevel >= 3 only. l3 is dataflow.Run's side table,
// nil below L3; see emit_l3.go for how it merges into each callable.
//
// This is a pure re-serialization: every fact comes from the v1 model and the
// L3 side table, except span byte offsets, which are derived from source via
// lineIndex.
func Emit(app *schema.GoApplication, appName, projectDir string, maxLevel int, analyzerVersion string, kLimit int, l3 map[dataflow.L3Key]dataflow.Result) *v2.Analysis {
	e := newEmitter(app, appName, projectDir, maxLevel, l3)
	out := e.envelope(app, maxLevel, analyzerVersion, kLimit)
	out.Application.SymbolTable = make(map[string]v2.Module, len(app.SymbolTable))
	for _, relPath := range sortedFileKeys(app.SymbolTable) {
		out.Application.SymbolTable[relPath] = e.module(relPath, app.SymbolTable[relPath])
	}
	return out
}

// EmitTo writes the analysis.json bytes of Emit's payload to w — the same bytes
// json.Marshal(Emit(...)) produces — without building the whole tree: each
// module is emitted, marshalled and dropped in symbol_table key order, so peak
// memory is the envelope plus the largest module, not the document.
func EmitTo(w io.Writer, app *schema.GoApplication, appName, projectDir string, maxLevel int, analyzerVersion string, kLimit int, l3 map[dataflow.L3Key]dataflow.Result) error {
	e := newEmitter(app, appName, projectDir, maxLevel, l3)
	out := e.envelope(app, maxLevel, analyzerVersion, kLimit)
	return encode(w, out, func(yield func(string, v2.Module) bool) {
		for _, relPath := range sortedFileKeys(app.SymbolTable) {
			if !yield(relPath, e.module(relPath, app.SymbolTable[relPath])) {
				return
			}
		}
	})
}

// Encode writes a to w as the exact bytes json.Marshal(a) produces (no
// trailing newline), streaming symbol_table one module at a time so the whole
// document never exists as one []byte.
func Encode(w io.Writer, a *v2.Analysis) error {
	st := a.Application.SymbolTable
	if st == nil { // json.Marshal writes null; nothing to stream
		data, err := json.Marshal(a)
		if err != nil {
			return err
		}
		_, err = w.Write(data)
		return err
	}
	keys := make([]string, 0, len(st))
	for k := range st {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	env := *a
	env.Application.SymbolTable = nil
	return encode(w, &env, func(yield func(string, v2.Module) bool) {
		for _, k := range keys {
			if !yield(k, st[k]) {
				return
			}
		}
	})
}

// symbolTableHole is the envelope's symbol_table as json.Marshal writes it
// when the map is empty. Every field before it is a number or a JSON string;
// inside a string every '"' is written as \", and the needle's second '"'
// follows 'e', so its first match is the field itself.
const symbolTableHole = `"symbol_table":{}`

// encode writes env (its SymbolTable ignored) with symbol_table filled from
// modules, which must yield keys in sort.Strings order — the order
// json.Marshal writes a map[string] in. Keys are marshalled as JSON strings,
// which is how encoding/json writes a string map key (same escaping).
func encode(w io.Writer, env *v2.Analysis, modules iter.Seq2[string, v2.Module]) error {
	shell := *env
	shell.Application.SymbolTable = map[string]v2.Module{}
	data, err := json.Marshal(&shell)
	if err != nil {
		return err
	}
	i := bytes.Index(data, []byte(symbolTableHole))
	if i < 0 {
		return fmt.Errorf("v2emit: no %s in the envelope", symbolTableHole)
	}
	split := i + len(symbolTableHole) - 1 // at the closing '}'
	if _, err := w.Write(data[:split]); err != nil {
		return err
	}
	first := true
	for k, m := range modules {
		if err := writeEntry(w, k, m, first); err != nil {
			return err
		}
		first = false
	}
	_, err = w.Write(data[split:])
	return err
}

func writeEntry(w io.Writer, key string, m v2.Module, first bool) error {
	k, err := json.Marshal(key)
	if err != nil {
		return err
	}
	v, err := json.Marshal(m)
	if err != nil {
		return err
	}
	buf := make([]byte, 0, len(k)+len(v)+2)
	if !first {
		buf = append(buf, ',')
	}
	buf = append(buf, k...)
	buf = append(buf, ':')
	if _, err := w.Write(buf); err != nil {
		return err
	}
	_, err = w.Write(v)
	return err
}

// emitter is the per-run state Emit and EmitTo share.
//
// The signature→can:// id index (idx) translates v1 identity (signature
// strings) into v2 node ids. It is what the L2 refinement uses to backfill
// body callees and to map call_graph edge endpoints onto real callable ids.
//
// srcs holds one lineIndex per source file, read once and shared. A method
// whose receiver type lives in a different file is nested under that type's
// module but must have its span computed against ITS OWN file (CLAUDE.md §
// Schema decisions → callable.source_file); the cache lets emitCallable reach
// any file's index without re-reading it per callable.
type emitter struct {
	appID string
	idx   sigIndex
	srcs  *sourceCache
	flow  l3Overlay
}

func newEmitter(app *schema.GoApplication, appName, projectDir string, maxLevel int, l3 map[dataflow.L3Key]dataflow.Result) *emitter {
	appID := v2.AppID(appName)
	idx := buildSigIndex(appID, app.SymbolTable)
	idx.externals = externalIDs(appID, app.Externals)
	return &emitter{appID: appID, idx: idx, srcs: newSourceCache(projectDir), flow: newL3Overlay(maxLevel, l3)}
}

// envelope is the payload without its symbol_table (nil): the manifest, the
// call graph and the external symbols.
func (e *emitter) envelope(app *schema.GoApplication, maxLevel int, analyzerVersion string, kLimit int) *v2.Analysis {
	out := &v2.Analysis{
		SchemaVersion: v2.SchemaVersion,
		Language:      v2.Language,
		MaxLevel:      maxLevel,
		Analyzer:      v2.AnalyzerTag{Name: "codeanalyzer-go", Version: analyzerVersion},
		Application: v2.Application{
			ID:        e.appID,
			Kind:      "application",
			CallGraph: emitCallGraph(app.CallGraph, e.idx),
		},
	}
	out.Application.ExternalSymbols = emitExternalSymbols(e.appID, app.Externals)
	if e.flow.on {
		out.KLimit = kLimit
	}
	return out
}

func (e *emitter) module(relPath string, file schema.GoFile) v2.Module {
	return emitModule(e.appID, relPath, file, e.idx, e.srcs, e.flow)
}

// emitModule builds a v2 module from a v1 GoFile, reading the file's source so
// spans can slice off it. A file that cannot be read still emits its structure
// (with empty source and zeroed byte spans) rather than dropping the module.
//
// modRel is this module's own file path (a symbol_table key). It is threaded to
// callables so a method declared in another file can be detected and given a
// span against its own source plus a source_file pointer.
func emitModule(appID, modRel string, file schema.GoFile, idx sigIndex, srcs *sourceCache, flow l3Overlay) v2.Module {
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
		mod.Types[name] = emitType(modID, modRel, t, idx, srcs, flow)
	}
	for sig, fn := range file.Functions {
		mod.Functions[sig] = emitCallable(modID, modRel, modRel, li, fn, idx, srcs, flow)
	}
	return mod
}

// emitType maps a v1 GoType to a v2 type node, collapsing is_interface into the
// kind and splitting embedding (base_types) from computed satisfaction. Methods
// resolve into the type's callables{}. modRel is the module the type lives in;
// its methods may be declared in other files (handled in emitCallable).
func emitType(modID, modRel string, t schema.GoType, idx sigIndex, srcs *sourceCache, flow l3Overlay) v2.Type {
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
		out.Callables[sig] = emitCallable(typeID, modRel, modRel, li, m, idx, srcs, flow)
	}
	return out
}

// emitCallable maps a v1 GoCallable to a v2 callable node, including its body
// (call nodes) and nested closures.
//
// modRel is the CONTAINING module's path: the module this callable nests
// under by containment (a method's receiver-type module, and for a closure the
// module its outermost enclosing callable nests under). declRel and declLI are
// the file the callable's positions are relative to and its lineIndex: the
// nesting file for a top-level callable, the enclosing callable's declaring
// file for a closure (Go has no cross-file closures; a v1 closure's Path is
// that same file). A callable's own c.Path, when set, is its declaring file.
//
// When the declaring file differs from the containing module — a method whose
// receiver type is in another file, and every closure (nested ones included)
// inside such a method — the callable records it as source_file, per CLAUDE.md
// § Schema decisions, and its spans index that file's source. Under the SDK's
// text-recovery rule (symbol_table[source_file].source when present, else the
// containing module's source) each of these nodes then slices the right file.
//
// At L3 the callable's dataflow result, looked up by (declaring file,
// signature), completes its body and adds cfg/cdg/ddg (emit_l3.go). Its spans
// use the same declaring-file lineIndex, so a cross-file callable's statement
// spans index its declaring file's source, like its own span.
func emitCallable(parentID, modRel, declRel string, declLI *lineIndex, c schema.GoCallable, idx sigIndex, srcs *sourceCache, flow l3Overlay) v2.Callable {
	callID := v2.CallableID(parentID, c.Signature)

	// Resolve the file this callable's positions are relative to.
	li := declLI
	if c.Path != "" && c.Path != declRel {
		li = srcs.index(c.Path)
		declRel = c.Path
	}
	sourceFile := ""
	if declRel != modRel {
		sourceFile = declRel
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
		// Closures nest in the same containing module and are declared in the
		// same file as their enclosing callable.
		for sig, inner := range c.InnerCallables {
			out.Callables[sig] = emitCallable(callID, modRel, declRel, li, inner, idx, srcs, flow)
		}
	}
	flow.apply(&out, li, dataflow.L3Key{Path: declRel, Signature: c.Signature})
	return out
}

// emitBody builds the body map: one `call` node per call site, keyed by its
// line:col local id, carrying the sanctioned callee null->id refinement slot.
// Every node carries its global id, v2.GlobalOrdinal(callable id, key), from L1
// on (R15; Python parity: codeanalyzer-python stamps body ids at L1).
//
// callee resolution:
//   - L1: the v1 site has no backfilled signature, so callee stays null.
//   - L2: the resolver backfilled the site's callee signature. When that
//     signature names an in-tree callable, callee becomes its can:// id; when
//     it names a library callee, callee becomes its @external id (a key of
//     application.external_symbols); otherwise (conversions, builtins, calls
//     through function values) callee stays null.
func emitBody(callableID string, li *lineIndex, sites []schema.GoCallsite, idx sigIndex) map[string]v2.BodyNode {
	keys := callBodyKeys(sites)
	body := make(map[string]v2.BodyNode, len(sites))
	for i, cs := range sites {
		local := keys[i]
		body[local] = v2.BodyNode{
			Kind:        "call",
			ID:          v2.GlobalOrdinal(callableID, local),
			Span:        colSpan(li, cs.StartLine, cs.StartColumn, cs.EndLine, cs.EndColumn),
			Callee:      calleeID(cs.CalleeSignature, idx),
			IsGoroutine: cs.IsGoroutine,
			IsDeferred:  cs.IsDeferred,
		}
	}
	return body
}

// callBodyKeys maps each call site to its body-map key. It is a thin wrapper:
// the co-positioned-call keying (l:c, l:c/2, …) lives in v2.CallBodyKeys so the
// L3 body builder shares it.
func callBodyKeys(sites []schema.GoCallsite) []string {
	pos := make([][2]int, len(sites))
	for i, cs := range sites {
		pos[i] = [2]int{cs.StartLine, cs.StartColumn}
	}
	return v2.CallBodyKeys(pos)
}

// calleeID translates a v1 backfilled callee signature into the v2 can:// id
// refinement slot: nil (JSON null) when unresolved, a pointer to the in-tree
// callable id or the library callee's @external id when resolved. Never returns
// the raw v1 signature — a call node's callee is always a node id or null.
func calleeID(sig *string, idx sigIndex) *string {
	if sig == nil {
		return nil
	}
	id, ok := idx.resolve(*sig)
	if !ok {
		return nil
	}
	return &id
}

// externalIDs maps each library callee's full signature to its @external id.
func externalIDs(appID string, externals map[string]schema.GoExternalCallee) map[string]string {
	ids := make(map[string]string, len(externals))
	for fullSig, ext := range externals {
		ids[fullSig] = v2.ExternalID(appID, ext.ImportPath, ext.Sig)
	}
	return ids
}

// emitExternalSymbols builds application.external_symbols, keyed by @external
// id. Nil (omitted from JSON) when no library callee was recorded.
func emitExternalSymbols(appID string, externals map[string]schema.GoExternalCallee) map[string]v2.ExternalSymbol {
	if len(externals) == 0 {
		return nil
	}
	out := make(map[string]v2.ExternalSymbol, len(externals))
	for _, ext := range externals {
		id := v2.ExternalID(appID, ext.ImportPath, ext.Sig)
		params := make([]v2.ExternalParameter, 0, len(ext.Params))
		for _, p := range ext.Params {
			params = append(params, v2.ExternalParameter{Name: p.Name, Type: p.Type, IsVariadic: p.IsVariadic})
		}
		sym := v2.ExternalSymbol{
			ID: id, Kind: "external", Name: ext.Name, Module: ext.ImportPath,
			Signature: ext.Sig, Parameters: params, ReturnType: ext.ReturnType,
		}
		if ext.Receiver != nil {
			sym.Receiver = &v2.ExternalParameter{Name: ext.Receiver.Name, Type: ext.Receiver.Type}
		}
		out[id] = sym
	}
	return out
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
// v1 signatures to can:// ids. A dst may be an in-tree callable or a library
// callee's @external id (a key of external_symbols). An edge whose src or dst
// does not resolve is dropped rather than emitted with a dangling endpoint —
// the L2 gate requires every endpoint to be a real node id.
func emitCallGraph(edges []schema.GoCallEdge, idx sigIndex) []v2.Edge {
	out := make([]v2.Edge, 0, len(edges))
	for _, e := range edges {
		// Resolve the source by (signature, declaring file): a package-level
		// init signature repeats once per file in a package, so the file is
		// what pins the edge to the init that actually contains the call site.
		// The target is signature-only unless the edge carries its declaring
		// file: a static call target is never an init (Go forbids calling
		// init), but a VTA target may be a closure of one, whose signature
		// repeats per file.
		src, srcOK := idx.idForWithPath(e.Source, e.SourcePath)
		dst, dstOK := idx.resolve(e.Target)
		if e.TargetPath != "" {
			dst, dstOK = idx.idForWithPath(e.Target, e.TargetPath)
		}
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

// typeKind reports the v2 kind (struct | interface | alias | defined), recorded
// by the symbol-table builder from the type declaration. The IsInterface
// fallback covers hand-built v1 types that carry no TypeKind.
func typeKind(t schema.GoType) string {
	if t.TypeKind != "" {
		return t.TypeKind
	}
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
