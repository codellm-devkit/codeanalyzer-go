package v2emit

import (
	"github.com/codellm-devkit/codeanalyzer-go/internal/schema"
	v2 "github.com/codellm-devkit/codeanalyzer-go/internal/schema/v2"
)

// sigIndex maps a v1 callable signature to its v2 can:// callable id.
//
// v1 identifies every callable by its signatureOf() string and uses that string
// as call-graph edge endpoints and as a call site's backfilled callee. v2
// addresses callables by durable can:// id. The L2 refinement therefore needs a
// signature→id translation: this index is built in one pass over the same tree
// the emitter walks, using the SAME id builders emitCallable uses, so an id here
// is byte-identical to the id the callable node carries. That is what lets the
// L2 gate assert "no dangling endpoints" — every edge endpoint resolves to a
// real node id.
//
// Signatures are ALMOST globally unique (a method signature embeds its receiver
// type), with one exception: package-level `func init()`. Go permits many init
// functions per package (one per file), and they all share the signature
// "<pkgpath>.init". They are nonetheless distinct callables with distinct
// can:// ids — the id embeds the declaring file via its parent module — so a
// flat signature→id map collapses them, and since it is populated by ranging a
// Go map (random order) the surviving id is nondeterministic run to run. That
// nondeterminism propagated into the call_graph edge set.
//
// The index therefore keeps two maps: a flat signature→id (deterministic on
// collision — smallest id wins) for the unique common case, and a
// (signature,path)→id map that disambiguates the init collision by the source
// callable's declaring file. See idFor / idForWithPath.
type sigIndex struct {
	bySig     map[string]string
	bySigPath map[sigPathKey]string
}

type sigPathKey struct {
	sig  string
	path string
}

// buildSigIndex walks the symbol table and records, for every callable
// (functions, methods, and nested closures), signature → can:// id. It mirrors
// emitModule/emitType/emitCallable's parent-id computation exactly.
func buildSigIndex(appID string, symbolTable map[string]schema.GoFile) sigIndex {
	idx := sigIndex{
		bySig:     make(map[string]string),
		bySigPath: make(map[sigPathKey]string),
	}
	for relPath, file := range symbolTable {
		modID := v2.ModuleID(appID, relPath)
		for _, fn := range file.Functions {
			idx.addCallable(modID, relPath, fn)
		}
		for _, t := range file.Types {
			typeID := v2.TypeID(modID, t.Signature)
			for _, m := range t.Methods {
				idx.addCallable(typeID, relPath, m)
			}
		}
	}
	return idx
}

// addCallable records one callable and recurses into its closures, matching the
// id nesting emitCallable produces. relPath is the module file this callable is
// reached through; it keys the disambiguating (signature,path) entry.
func (idx sigIndex) addCallable(parentID, relPath string, c schema.GoCallable) {
	callID := v2.CallableID(parentID, c.Signature)

	// Flat entry: deterministic on collision — keep the lexicographically
	// smallest id so a repeated signature (package-level init) resolves to a
	// stable node even when the only information available is the bare
	// signature. (The (sig,path) map below is the precise resolver when a path
	// is known.)
	if existing, ok := idx.bySig[c.Signature]; !ok || callID < existing {
		idx.bySig[c.Signature] = callID
	}

	// Precise entry: a callable's declaring file plus its signature is unique
	// even for init, because at most one init exists per file.
	idx.bySigPath[sigPathKey{sig: c.Signature, path: relPath}] = callID

	for _, inner := range c.InnerCallables {
		idx.addCallable(callID, relPath, inner)
	}
}

// idFor returns the can:// id for a v1 signature, or ("", false) when the
// signature names a callable outside the project (stdlib/external) — those
// resolve to no in-tree node and must not become an edge endpoint. For a
// signature shared by multiple callables (package-level init) it returns the
// deterministic smallest id; callers that know the declaring file should use
// idForWithPath for the precise node.
func (idx sigIndex) idFor(sig string) (string, bool) {
	id, ok := idx.bySig[sig]
	return id, ok
}

// idForWithPath returns the can:// id for a signature declared in a specific
// file. It is the precise resolver for a non-unique signature: an init call
// site in request_id.go resolves to request_id.go's init node, not an
// arbitrary sibling's. When path is empty or no (sig,path) entry exists it
// falls back to idFor (the deterministic flat lookup), so unique signatures and
// edges that carry no source path still resolve.
func (idx sigIndex) idForWithPath(sig, path string) (string, bool) {
	if path != "" {
		if id, ok := idx.bySigPath[sigPathKey{sig: sig, path: path}]; ok {
			return id, true
		}
	}
	return idx.idFor(sig)
}
