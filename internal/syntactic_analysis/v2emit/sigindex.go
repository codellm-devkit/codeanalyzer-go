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
// v1 signatures are globally unique (a method signature embeds its receiver
// type), so a flat signature→id map has no collisions.
type sigIndex map[string]string

// buildSigIndex walks the symbol table and records, for every callable
// (functions, methods, and nested closures), signature → can:// id. It mirrors
// emitModule/emitType/emitCallable's parent-id computation exactly.
func buildSigIndex(appID string, symbolTable map[string]schema.GoFile) sigIndex {
	idx := make(sigIndex)
	for relPath, file := range symbolTable {
		modID := v2.ModuleID(appID, relPath)
		for _, fn := range file.Functions {
			idx.addCallable(modID, fn)
		}
		for _, t := range file.Types {
			typeID := v2.TypeID(modID, t.Signature)
			for _, m := range t.Methods {
				idx.addCallable(typeID, m)
			}
		}
	}
	return idx
}

// addCallable records one callable and recurses into its closures, matching the
// id nesting emitCallable produces.
func (idx sigIndex) addCallable(parentID string, c schema.GoCallable) {
	callID := v2.CallableID(parentID, c.Signature)
	idx[c.Signature] = callID
	for _, inner := range c.InnerCallables {
		idx.addCallable(callID, inner)
	}
}

// idFor returns the can:// id for a v1 signature, or ("", false) when the
// signature names a callable outside the project (stdlib/external) — those
// resolve to no in-tree node and must not become an edge endpoint.
func (idx sigIndex) idFor(sig string) (string, bool) {
	id, ok := idx[sig]
	return id, ok
}
