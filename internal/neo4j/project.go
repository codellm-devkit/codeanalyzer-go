package neo4j

import (
	"encoding/json"

	v2 "github.com/codellm-devkit/codeanalyzer-go/internal/schema/v2"
)

// Milestone M3: the pure projector. Project walks the canonical v2 Analysis
// tree and emits a GraphRows bag — one graph node per tree/body node, keyed on
// its can:// id, with containment and every typed overlay as relationships. No
// I/O, no driver; both writers consume the result identically.
//
// The projector NEVER invents ids: it reads them off the stamped v2 tree, so
// the graph and analysis.json join on one string (two-projection agreement).
// Node ids are can:// ids; the application segment is the outermost prefix of
// every id the app owns, which is what makes prefix scoping exact.
//
// Walk order (spine): application → per module (types → callables → fields →
// body call nodes) → call_graph → imports → neutral artifact/package layer →
// reserved L3/L4 overlay (zero rows at the current max level). Every endpoint
// resolves to a declared node or an id-keyed :GoExternal ghost — nothing dangles.

// Project is the pure (tree) → GraphRows function. appID is analysis.Application.ID
// (the can://<lang>/<app> root); it is read from the envelope, never recomposed.
func Project(a *v2.Analysis) GraphRows {
	p := &projector{
		b:        NewRowBuilder(),
		analysis: a,
		// Index of every emitted callable/type id, so call_graph endpoints and
		// body callees can tell a declared target from an external one.
		declared: collectDeclaredIDs(a),
	}
	p.projectApplication()
	for _, mod := range a.Application.SymbolTable {
		p.projectModule(mod)
	}
	p.projectCallGraph()
	return p.b.Finish()
}

type projector struct {
	b        *RowBuilder
	analysis *v2.Analysis
	declared map[string]bool
}

// ─── application ─────────────────────────────────────────────────────────────

func (p *projector) projectApplication() {
	a := p.analysis
	p.b.Node(
		[]string{LabelApplication, MarkerLabel},
		"id", a.Application.ID,
		Props{
			"schema_version":   a.SchemaVersion,
			"language":         a.Language,
			"max_level":        a.MaxLevel,
			"analyzer_name":    a.Analyzer.Name,
			"analyzer_version": a.Analyzer.Version,
		},
		"", // the application root belongs to no single module
	)
}

func (p *projector) appRef() NodeRef {
	return NodeRef{Label: LabelApplication, KeyProp: "id", Value: p.analysis.Application.ID}
}

// ─── module ──────────────────────────────────────────────────────────────────

func (p *projector) projectModule(m v2.Module) {
	modRef := p.b.Node(
		[]string{LabelModule, MarkerLabel},
		"id", m.ID,
		Props{
			"package":      m.Package,
			"source":       m.Source,
			"content_hash": emptyToNil(m.ContentHash),
			"span_json":    spanJSON(m.Span),
		},
		m.ID,
	)
	p.b.Edge(RelHasModule, p.appRef(), modRef, nil, "")

	for _, t := range m.Types {
		p.projectType(modRef, m.ID, t)
	}
	for _, fn := range m.Functions {
		cRef := p.projectCallable(modRef, m.ID, fn)
		p.b.Edge(RelDeclares, modRef, cRef, nil, "")
	}
	p.projectImports(modRef, m)
}

// ─── type ─────────────────────────────────────────────────────────────────────

func (p *projector) projectType(modRef NodeRef, module string, t v2.Type) {
	tRef := p.b.Node(
		[]string{SymbolLabel, LabelType, MarkerLabel},
		"id", t.ID,
		Props{
			"kind":      t.Kind, // struct|interface|alias|defined (decision T1)
			"signature": lastSegment(t.ID),
			"span_json": spanJSON(t.Span),
		},
		module,
	)
	p.b.Edge(RelDeclares, modRef, tRef, nil, "")

	// Embedding (explicit spine): deferred — a base type may be external.
	for _, baseID := range t.BaseTypes {
		p.b.EdgeToSymbol(RelEmbeds, tRef, SymbolLabel, "id", baseID, nil, "")
	}
	// Structural satisfaction (computed). The interfaces are in-tree type ids
	// the analyzer computed; still routed through the deferred path so a
	// satisfied-but-external interface never dangles.
	for _, ifaceID := range t.Interfaces {
		p.b.EdgeToSymbol(RelSatisfies, tRef, SymbolLabel, "id", ifaceID, nil, "")
	}

	// Struct fields.
	for _, f := range t.Fields {
		fRef := p.b.Node(
			[]string{LabelField, MarkerLabel},
			"id", f.ID,
			Props{"kind": f.Kind, "type": f.Type, "span_json": spanJSON(f.Span)},
			module,
		)
		p.b.Edge(RelHasField, tRef, fRef, nil, "")
	}

	// Methods nest under their receiver type (containment-by-receiver).
	for _, m := range t.Callables {
		mRef := p.projectCallable(tRef, module, m)
		p.b.Edge(RelHasMethod, tRef, mRef, nil, "")
	}
}

// ─── callable ─────────────────────────────────────────────────────────────────

// projectCallable emits a callable node (+ its body call nodes and nested
// closures) and returns its ref. The containment edge from the parent (DECLARES
// or HAS_METHOD) is the caller's responsibility, since it differs by parent kind.
func (p *projector) projectCallable(parentRef NodeRef, module string, c v2.Callable) NodeRef {
	cRef := p.b.Node(
		[]string{SymbolLabel, LabelCallable, MarkerLabel},
		"id", c.ID,
		Props{
			"kind":          c.Kind,
			"signature":     c.Signature,
			"return_type":   emptyToNil(c.ReturnType),
			"error_channel": stringsOrNil(c.ErrorChannel),
			"source_file":   emptyToNil(c.SourceFile), // only when ≠ nesting module (D8)
			"span_json":     spanJSON(c.Span),
			"metrics_json":  metricsJSON(c.Metrics),
		},
		module,
	)

	// Body call nodes (L1). Keyed by the global ordinal id (<callable-id>@<local>)
	// so graph and JSON body nodes land on one identity.
	for local, bn := range c.Body {
		p.projectBodyNode(cRef, module, c.ID, local, bn)
	}

	// Nested closures → GO_DECLARES (callable → closure).
	for _, inner := range c.Callables {
		innerRef := p.projectCallable(cRef, module, inner)
		p.b.Edge(RelDeclares, cRef, innerRef, nil, "")
	}
	return cRef
}

func (p *projector) projectBodyNode(callableRef NodeRef, module, callableID, local string, bn v2.BodyNode) {
	id := callableID + "@" + local
	bRef := p.b.Node(
		[]string{LabelBodyNode, MarkerLabel},
		"id", id,
		Props{
			"kind":         bn.Kind,
			"is_goroutine": boolOrNil(bn.IsGoroutine),
			"is_deferred":  boolOrNil(bn.IsDeferred),
			"span_json":    spanJSON(bn.Span),
		},
		module,
	)
	p.b.Edge(RelHasBodyNode, callableRef, bRef, nil, "")

	// A resolved callee (backfilled at L2) → GO_RESOLVES_TO. The callee is
	// already a resolved can:// id; route through the plain edge only when it
	// names a declared callable (an external callee stays null in JSON and
	// emits no edge here).
	if bn.Callee != nil && p.declared[*bn.Callee] {
		p.b.Edge(RelResolvesTo, bRef,
			NodeRef{Label: SymbolLabel, KeyProp: "id", Value: *bn.Callee}, nil, "")
	}
}

// ─── imports ──────────────────────────────────────────────────────────────────

// projectImports aggregates a module's imports into one GO_IMPORTS edge per
// resolved target. An import whose path names an in-tree module resolves to that
// :GoModule; otherwise it resolves to an id-keyed :GoExternal ghost (stdlib /
// third-party), which never dangles.
func (p *projector) projectImports(modRef NodeRef, m v2.Module) {
	appID := p.analysis.Application.ID
	for _, imp := range m.Imports {
		// Is the import path an in-project module? (symbol_table is keyed by
		// relative file path; Go imports name a package path, not a file — so an
		// in-project hit is rare and handled conservatively via the id space.)
		targetModuleID := v2.ModuleID(appID, imp.Path)
		if _, ok := p.analysis.Application.SymbolTable[imp.Path]; ok {
			p.b.Edge(RelImports, modRef,
				NodeRef{Label: LabelModule, KeyProp: "id", Value: targetModuleID},
				Props{"alias": emptyToNil(imp.Alias)}, imp.Path)
			continue
		}
		// External/stdlib → :GoExternal ghost keyed by import path.
		ghostID := appID + "/@external/" + imp.Path
		ghostRef := p.b.Node(
			[]string{SymbolLabel, LabelExternal, MarkerLabel},
			"id", ghostID,
			Props{"name": imp.Name, "path": imp.Path},
			"", // shared — belongs to no owning module
		)
		p.b.Edge(RelImports, modRef, ghostRef, Props{"alias": emptyToNil(imp.Alias)}, imp.Path)
	}
}

// ─── call graph ─────────────────────────────────────────────────────────────

// projectCallGraph emits the aggregated GO_CALLS edges. Endpoints are already
// can:// callable ids in the v2 edge; a declared id resolves to its callable
// node, anything else to an id-keyed external ghost — so no GO_CALLS endpoint
// ever dangles.
func (p *projector) projectCallGraph() {
	for _, e := range p.analysis.Application.CallGraph {
		from := p.callEndpoint(e.Src)
		to := p.callEndpoint(e.Dst)
		p.b.Edge(RelCalls, from, to, Props{
			"weight": intOrNil(e.Weight),
			"prov":   stringsOrNil(e.Prov),
		}, "")
	}
}

// callEndpoint resolves a call endpoint id to a NodeRef. A declared callable
// addresses its node by the shared symbol label; an undeclared id is minted as
// an external ghost node (so the MATCH finds something at load time).
func (p *projector) callEndpoint(id string) NodeRef {
	ref := NodeRef{Label: SymbolLabel, KeyProp: "id", Value: id}
	if p.declared[id] {
		return ref
	}
	// Emit the ghost so the endpoint is a real node, not a dangling MATCH.
	p.b.Node([]string{SymbolLabel, LabelExternal, MarkerLabel}, "id", id,
		Props{"name": lastSegment(id)}, "")
	return ref
}

// ─── id collection ─────────────────────────────────────────────────────────

// collectDeclaredIDs walks the tree once to gather every declared type and
// callable id, so endpoint resolution can distinguish a declared target from an
// external one without emitting it twice.
func collectDeclaredIDs(a *v2.Analysis) map[string]bool {
	ids := make(map[string]bool)
	var walkCallable func(c v2.Callable)
	walkCallable = func(c v2.Callable) {
		ids[c.ID] = true
		for _, inner := range c.Callables {
			walkCallable(inner)
		}
	}
	for _, m := range a.Application.SymbolTable {
		for _, t := range m.Types {
			ids[t.ID] = true
			for _, meth := range t.Callables {
				walkCallable(meth)
			}
		}
		for _, fn := range m.Functions {
			walkCallable(fn)
		}
	}
	return ids
}

// ─── flattening helpers ──────────────────────────────────────────────────────

// spanJSON flattens a span to a compact JSON string property. (A span is a
// nested map, which Neo4j cannot store as a property, so it rides as *_json —
// the same approach the siblings use for variant-shaped fields.)
func spanJSON(s v2.Span) Value {
	data, err := json.Marshal(s)
	if err != nil {
		return nil
	}
	return string(data)
}

func metricsJSON(m map[string]int) Value {
	if len(m) == 0 {
		return nil
	}
	data, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return string(data)
}

// emptyToNil returns nil for "" so prune() drops the property (absence = no fact).
func emptyToNil(s string) Value {
	if s == "" {
		return nil
	}
	return s
}

// boolOrNil returns nil for false so a false flag is simply absent (matching the
// JSON omitempty on is_goroutine/is_deferred).
func boolOrNil(b bool) Value {
	if !b {
		return nil
	}
	return true
}

func intOrNil(n int) Value {
	if n == 0 {
		return nil
	}
	return n
}

func stringsOrNil(s []string) Value {
	if len(s) == 0 {
		return nil
	}
	return s
}

// lastSegment returns the final "/" or "@"-delimited segment of a can:// id,
// used as a human-readable signature/name fallback for ghosts.
func lastSegment(id string) string {
	last := id
	for i := len(id) - 1; i >= 0; i-- {
		if id[i] == '/' || id[i] == '@' {
			last = id[i+1:]
			break
		}
	}
	return last
}
