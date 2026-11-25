package neo4j

import (
	"iter"
	"maps"
	"slices"

	v2 "github.com/codellm-devkit/codeanalyzer-go/internal/schema/v2"
)

// stager consumes the projection in push order (spec §5.3): every node stage
// runs before the first edge stage, so no edge MATCH misses its endpoint.
//
// A stager must not keep a slice passed to module, shared, late or edges after
// it returns: the caller may reuse it (streamParamEdges reuses its chunk).
// Copying the row values out, for example with append(buf, rows...), is fine.
type stager interface {
	begin(app NodeRow) error                 // step 0: identity, stale check, DDL, eager purge
	shared(nodes []NodeRow) error            // step 1: repository nodes
	module(id string, nodes []NodeRow) error // step 2: every node row of one module
	late(nodes []NodeRow) error              // step 3: ghosts, stand-ins, application row
	edges(edges []EdgeRow) error             // step 4: called many times
	end(moduleIDs []string) error            // step 5: flush, prune
}

// streamGraph projects src into st in two module walks. Pass 1 sends the node
// rows of each module and collects the declared ids; the ghosts, stand-ins and
// the application row follow once that set is complete. Pass 2 sends the edge
// rows. No module step holds more than one module's rows, and the param edges
// go in chunks; the late and tail builders are app-wide, though: they hold
// every GO_CALLS row, call-graph and callee ghost, and stand-in.
// Across modules a duplicate edge row may reach st; MERGE makes it a no-op.
func streamGraph(src Source, st stager) (nodes, edges int, err error) {
	env := src.Envelope()
	keys := src.ModuleKeys()
	modSet := keySet(keys)
	calleeIDs := map[string]bool{}
	mk := func(b *RowBuilder, declared map[string]bool) *projector {
		return &projector{b: b, analysis: env, modules: modSet, declared: declared, calleeIDs: calleeIDs}
	}
	// Step 0. The identity check needs the application row before any write.
	// Its degraded_json is not known yet, so begin gets the row without it.
	if err = st.begin(appRow(env)); err != nil {
		return
	}
	// Step 1: repository nodes.
	repo := mk(nodeOnly(), nil)
	repo.projectRepository()
	shared := repo.b.Finish().Nodes
	if err = st.shared(shared); err != nil {
		return
	}
	nodes += len(shared)
	// Step 2, pass 1: module nodes. Ghost nodes (Module == "") move to the late builder.
	declared := map[string]bool{}
	late := mk(nodeOnly(), declared)
	for _, m := range src.Modules() {
		moduleDeclared(m, declared)
		p := mk(nodeOnly(), declared)
		p.projectModule(m)
		var own []NodeRow
		for _, n := range p.b.Finish().Nodes {
			if n.Module == "" {
				late.b.Node(n.Labels, n.KeyProp, n.Value, n.Props, "")
				continue
			}
			own = append(own, n)
		}
		if err = st.module(m.ID, own); err != nil {
			return
		}
		nodes += len(own)
	}
	// Step 3: against the complete declared set, the body-callee ghosts, the
	// call-graph ghosts, the stand-in vertices, and the application row, which
	// now carries degraded_json.
	for _, id := range slices.Sorted(maps.Keys(calleeIDs)) {
		late.callEndpoint(id)
	}
	late.projectCallGraph()
	late.projectStandIns()
	late.projectApplication(src.Degraded())
	lateRows := late.b.Finish().Nodes
	if err = st.late(lateRows); err != nil {
		return
	}
	nodes += len(lateRows)

	// Step 4, pass 2: edges. GO_EMBEDS / GO_SATISFIES resolve against every
	// :GoSymbol id: the declared ids and the late ghosts.
	symbols := maps.Clone(declared)
	for _, n := range lateRows {
		if slices.Contains(n.Labels, SymbolLabel) {
			symbols[n.Value] = true
		}
	}
	for _, m := range src.Modules() {
		p := mk(NewRowBuilder(), declared)
		p.projectModule(m)
		r := p.b.finishWith(symbols)
		if err = st.edges(r.Edges); err != nil {
			return
		}
		edges += len(r.Edges)
	}
	// The repository edges, GO_CALLS and the stand-in GO_HAS_BODY_NODE edges.
	// No deferred edge here: EdgeToSymbol only runs in projectType.
	tail := mk(NewRowBuilder(), declared)
	tail.projectRepository()
	tail.projectCallGraph()
	tail.projectStandIns()
	r := tail.b.Finish()
	if err = st.edges(r.Edges); err != nil {
		return
	}
	edges += len(r.Edges)
	n, err := streamParamEdges(src, st)
	edges += n
	if err != nil {
		return
	}
	// Step 5.
	moduleIDs := make([]string, len(keys))
	for i, k := range keys {
		moduleIDs[i] = v2.ModuleID(env.Application.ID, k)
	}
	err = st.end(moduleIDs)
	return
}

// streamParamEdges sends GO_PARAM_IN then GO_PARAM_OUT to st in chunks of
// boltBatchSize. The param edges bypass every RowBuilder: the emitter already
// sorts and deduplicates them, and on a large app their ids are gigabytes.
// The chunk is reused, so st must not keep it (see stager).
func streamParamEdges(src Source, st stager) (int, error) {
	chunk := make([]EdgeRow, 0, boltBatchSize)
	sent := 0
	flush := func() error {
		if len(chunk) == 0 {
			return nil
		}
		if err := st.edges(chunk); err != nil {
			return err
		}
		sent += len(chunk)
		chunk = chunk[:0]
		return nil
	}
	for _, s := range []struct {
		rel   string
		edges iter.Seq[v2.ParamEdge]
	}{{RelParamIn, src.ParamIn()}, {RelParamOut, src.ParamOut()}} {
		for e := range s.edges {
			chunk = append(chunk, paramEdgeRow(s.rel, e))
			if len(chunk) == boltBatchSize {
				if err := flush(); err != nil {
					return sent, err
				}
			}
		}
	}
	return sent, flush()
}

// replayRows feeds a ready GraphRows through the stages of st: the
// application row to begin (and again to shared, with the other module-less
// rows), each module's rows (its L4 vertices included) in module order, the
// stand-in vertices to late, then every edge in one call.
func replayRows(rows GraphRows, st stager) (nodes, edges int, err error) {
	var app NodeRow
	var shared, late []NodeRow
	byModule := map[string][]NodeRow{}
	for _, n := range rows.Nodes {
		if n.Labels[0] == LabelApplication {
			app = n
		}
		switch {
		case n.Module == "" && isL4Node(n):
			late = append(late, n)
		case n.Module == "":
			shared = append(shared, n)
		default:
			byModule[n.Module] = append(byModule[n.Module], n)
		}
	}
	if err = st.begin(app); err != nil {
		return
	}
	if err = st.shared(shared); err != nil {
		return
	}
	nodes += len(shared)
	ids := sortedModuleKeys(byModule)
	for _, id := range ids {
		if err = st.module(id, byModule[id]); err != nil {
			return
		}
		nodes += len(byModule[id])
	}
	if err = st.late(late); err != nil {
		return
	}
	nodes += len(late)
	if err = st.edges(rows.Edges); err != nil {
		return
	}
	edges = len(rows.Edges)
	err = st.end(ids)
	return
}

// appRow is the application row projectApplication makes, without
// degraded_json, which is known only after the module walk.
func appRow(env *v2.Analysis) NodeRow {
	p := &projector{b: NewRowBuilder(), analysis: env}
	p.projectApplication(nil)
	return p.b.Finish().Nodes[0]
}

// nodeOnly returns a builder that drops every edge (pass 1).
func nodeOnly() *RowBuilder {
	b := NewRowBuilder()
	b.noEdges = true
	return b
}

func keySet(keys []string) map[string]bool {
	s := make(map[string]bool, len(keys))
	for _, k := range keys {
		s[k] = true
	}
	return s
}
