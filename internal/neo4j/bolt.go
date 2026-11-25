package neo4j

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sort"

	v2 "github.com/codellm-devkit/codeanalyzer-go/internal/schema/v2"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

// Milestone M5: the incremental Bolt writer. It pushes a GraphRows bag into a
// live Neo4j over Bolt, updating ONLY what changed, with the module subgraph as
// the unit of idempotent replacement. The neo4j driver is pure Go, so a
// CGO_ENABLED=0 static build still links; it is only exercised when EmitNeo4j
// runs with a --neo4j-uri, never on the JSON path.
//
// The writer is a stager (stream.go); its stages run in push order (spec §5.3),
// numbered as the stager numbers them:
//  0. begin: stored-application gate — read :GoApplication.schema_version and
//     max_level scoped to this app's id; a version mismatch, or a stored level
//     below this push's, forces a full re-UPSERT (never a delete — that is
//     --eager's job); ensure constraints + indexes; --eager? scoped purge of
//     everything under the app prefix, then rebuild;
//  1. shared: upsert the module-less repository nodes (MERGE-only, always);
//  2. module, per module: diff its content_hash against the DB; when changed,
//     on --eager purge its owned edges + stale decls, then upsert its L1–L3
//     nodes (WITHOUT --eager the push only adds/updates) with content_hash
//     cleared; upsert its L4 vertex nodes, changed module or not (D5, below);
//  3. late: upsert the ghosts, stand-in vertices and application row, flush
//     every node buffer, then stamp the content_hash of every module upserted
//     in step 2;
//  4. edges: upsert every edge (an unchanged edge's MERGE is a no-op);
//  5. end: flush the edges; on a full --eager run, orphan-prune modules whose
//     source vanished.
//
// The content_hash is stamped only after every node row of every module has
// landed. Node groups flush in group-key order and in boltBatchSize chunks, so
// a :GoModule row can land before the rest of its module's nodes; a hash
// written with it would survive a push that dies before those nodes land, the
// next lazy push would skip the module as unchanged, and the edges into its
// missing nodes would be dropped by MATCH. The :GoModule row is therefore
// written with content_hash = null (clearing a stored hash) and the hash set
// by batched UNWIND statements (boltBatchSize rows each) after the final node
// flush. Edges need no such
// guard: every push upserts every edge.
//
// D5 (spec §4.11): L4 facts are not module-local — editing a callee changes
// its callers' summary edges, actual vertices and even formal_in indices —
// so the content_hash diff gates the L1–L3 rows only. A lazy push upserts
// every L4 row on every push: the vertex nodes (steps 2–3), the stand-in
// bodies among them, and the GO_PARAM_IN / GO_PARAM_OUT / GO_SUMMARY rows
// and the port GO_DDG rows (prov ≠ ssa) with every other edge (step 4). It
// is additive: a stale L4 row stays until an --eager push.
//
// A push NEVER deletes by default (#171): the purges and the prune run under
// --eager only.
// Every destructive statement is scoped on the can://<namespace>/<app>/ id prefix (#173),
// anchored on :GoCanNode so it seeks an index.

// BoltConfig carries the connection + behavior knobs (populated by the facade
// from AnalysisOptions).
type BoltConfig struct {
	URI      string
	User     string
	Password string
	Database string
	// Eager enables the destructive reconciliation steps. Default (lazy) is
	// purely additive: MERGE-upsert, nothing removed.
	Eager bool
	// FullRun is true when the analysis saw every file (not a --target-files
	// subset); orphan pruning only runs on a full, eager run.
	FullRun bool
}

// boltBatchSize is the most rows one UNWIND write transaction carries (#59).
// A whole group in one transaction can exceed the Neo4j transaction memory
// limit on a large app. The driver also encodes each message in client memory.
const boltBatchSize = 5000

// WriteBolt pushes ready rows into the live DB per BoltConfig. appID scopes
// every destructive statement; it must be the application root id. Tests and
// small graphs use it.
func WriteBolt(ctx context.Context, rows GraphRows, appID string, cfg BoltConfig) error {
	if err := validateIdentity(rows, appID); err != nil {
		return err
	}
	return withSession(ctx, cfg, func(s neo4j.SessionWithContext) error {
		_, _, err := replayRows(rows, newBoltWriter(ctx, s, appID, cfg))
		return err
	})
}

// PushBolt streams src into the DB in the two-pass order (spec §5.3). It
// returns the row counts.
func PushBolt(ctx context.Context, src Source, cfg BoltConfig) (nodes, edges int, err error) {
	env := src.Envelope()
	appID := env.Application.ID
	if err = validateIdentity(GraphRows{Nodes: []NodeRow{appRow(env)}}, appID); err != nil {
		return
	}
	err = withSession(ctx, cfg, func(s neo4j.SessionWithContext) error {
		nodes, edges, err = streamGraph(src, newBoltWriter(ctx, s, appID, cfg))
		return err
	})
	return
}

// withSession opens a driver and a session per cfg, runs fn, and closes both.
func withSession(ctx context.Context, cfg BoltConfig, fn func(neo4j.SessionWithContext) error) error {
	driver, err := neo4j.NewDriverWithContext(cfg.URI, neo4j.BasicAuth(cfg.User, cfg.Password, ""))
	if err != nil {
		return fmt.Errorf("neo4j driver: %w", err)
	}
	defer func() { _ = driver.Close(ctx) }()
	if err := driver.VerifyConnectivity(ctx); err != nil {
		return fmt.Errorf("neo4j connectivity: %w", err)
	}

	session := driver.NewSession(ctx, neo4j.SessionConfig{DatabaseName: cfg.Database})
	defer func() { _ = session.Close(ctx) }()
	return fn(session)
}

// boltWriter is the Bolt stager. Node rows are buffered per MERGE group and a
// group runs once it holds boltBatchSize rows; every node buffer is flushed by
// late, before the first edge statement, so no edge MATCH misses its endpoint.
type boltWriter struct {
	ctx     context.Context
	session neo4j.SessionWithContext
	appID   string
	cfg     BoltConfig
	level   int  // max_level of the begin row
	forced  bool // forceFull || cfg.Eager: every module counts as changed
	scoped  bool
	nodeBuf map[string][]NodeRow
	edgeBuf map[string][]EdgeRow
	// hashes are the content_hash values of the modules whose nodes were
	// upserted, stamped by late after the final node flush. Only the
	// (id, hash) pairs are kept, never the :GoModule rows (they carry source).
	hashes []moduleHashRow
}

// moduleHashRow is one deferred content_hash stamp.
type moduleHashRow struct{ id, hash string }

func newBoltWriter(ctx context.Context, s neo4j.SessionWithContext, appID string, cfg BoltConfig) *boltWriter {
	return &boltWriter{ctx: ctx, session: s, appID: appID, cfg: cfg,
		nodeBuf: map[string][]NodeRow{}, edgeBuf: map[string][]EdgeRow{}}
}

// begin runs the stored-application gate, the DDL and the eager purge. A
// producer/consumer version mismatch, or a graph pushed at a lower level,
// forces a full re-upsert (it never deletes): the per-module content_hash
// diff is not trusted and every module is upserted.
func (w *boltWriter) begin(app NodeRow) error {
	w.level, _ = app.Props["max_level"].(int)
	forceFull, err := w.storedAppStale()
	if err != nil {
		return err
	}
	if err := w.ensureDDL(); err != nil {
		return err
	}
	root, scoped := applicationPrefix(w.appID)
	w.scoped = scoped
	if w.shouldEagerPurge(scoped) {
		if err := w.eagerPurge(root); err != nil {
			return err
		}
	}
	w.forced = forceFull || w.cfg.Eager
	return nil
}

// shared upserts the module-less nodes (MERGE-only, every run).
func (w *boltWriter) shared(nodes []NodeRow) error {
	if err := w.bufferNodes(nodes); err != nil {
		return err
	}
	return w.flushNodes()
}

// module buffers one module's L1–L3 nodes when it changed (on --eager, after
// purging its owned edges and stale decls), and its L4 vertex nodes always,
// after the purge so it cannot delete them (D5).
func (w *boltWriter) module(id string, nodes []NodeRow) error {
	var core, l4 []NodeRow
	for _, n := range nodes {
		if isL4Node(n) {
			l4 = append(l4, n)
		} else {
			core = append(core, n)
		}
	}
	changed := w.forced
	if !changed {
		var err error
		if changed, err = w.moduleChanged(id, moduleHash(core)); err != nil {
			return err
		}
	}
	if changed {
		if w.cfg.Eager && w.scoped {
			// Flushed first so the DB order matches the statement order;
			// the purge only matches ids under this module's prefix.
			if err := w.flushNodes(); err != nil {
				return err
			}
			if err := w.purgeModule(id); err != nil {
				return err
			}
		}
		if err := w.bufferNodes(w.deferHashes(core)); err != nil {
			return err
		}
	}
	return w.bufferNodes(l4)
}

// deferHashes returns core with each :GoModule row's content_hash replaced by
// an explicit null (SET += removes a stored hash) and records the hash for
// stampHashes. The rows passed in are not modified: the caller may reuse them.
func (w *boltWriter) deferHashes(core []NodeRow) []NodeRow {
	var out []NodeRow
	for i, n := range core {
		h, ok := n.Props["content_hash"].(string)
		if n.Labels[0] != LabelModule || !ok {
			continue
		}
		if out == nil {
			out = slices.Clone(core)
		}
		p := maps.Clone(n.Props)
		p["content_hash"] = nil
		out[i].Props = p
		w.hashes = append(w.hashes, moduleHashRow{n.Value, h})
	}
	if out == nil {
		return core
	}
	return out
}

// late upserts the ghosts, the stand-in vertices and the application row,
// flushes every node buffer before the first edge statement, and then stamps
// the deferred module content_hash values (see the package doc).
func (w *boltWriter) late(nodes []NodeRow) error {
	if err := w.bufferNodes(nodes); err != nil {
		return err
	}
	if err := w.flushNodes(); err != nil {
		return err
	}
	return w.stampHashes()
}

// stampHashes sets the content_hash of every module recorded by deferHashes,
// in batches of boltBatchSize, once all node rows have landed.
func (w *boltWriter) stampHashes() error {
	const q = "UNWIND $rows AS r MATCH (m:GoModule {id: r.id}) SET m.content_hash = r.h"
	for chunk := range slices.Chunk(w.hashes, boltBatchSize) {
		rows := make([]map[string]any, len(chunk))
		for i, h := range chunk {
			rows[i] = map[string]any{"id": h.id, "h": h.hash}
		}
		if err := w.runBatch(q, rows); err != nil {
			return err
		}
	}
	w.hashes = nil
	return nil
}

// edges buffers every edge: the L4 rows must be upserted (D5), and for the
// L1–L3 rows the precise owner buys nothing — a MERGE-upsert of an unchanged
// edge is a harmless no-op.
func (w *boltWriter) edges(edges []EdgeRow) error {
	return w.bufferEdges(edges)
}

// end flushes the edges and, on a full --eager run, prunes vanished modules.
func (w *boltWriter) end(moduleIDs []string) error {
	if err := w.flushEdges(); err != nil {
		return err
	}
	if w.cfg.Eager && w.cfg.FullRun && w.scoped {
		return w.pruneVanishedModules(moduleIDs)
	}
	return nil
}

// ─── DDL + version gate ───────────────────────────────────────────────────────

func (w *boltWriter) ensureDDL() error {
	return w.write(func(tx neo4j.ManagedTransaction) error {
		for _, c := range UniquenessConstraints() {
			q := fmt.Sprintf("CREATE CONSTRAINT %s IF NOT EXISTS FOR (n:%s) REQUIRE n.%s IS UNIQUE",
				c.Name, c.Label, c.Property)
			if _, err := tx.Run(w.ctx, q, nil); err != nil {
				return err
			}
		}
		for _, idx := range Indexes() {
			q := fmt.Sprintf("CREATE INDEX %s IF NOT EXISTS FOR (n:%s) ON (n.%s)",
				idx.Name, idx.Label, idx.Property)
			if _, err := tx.Run(w.ctx, q, nil); err != nil {
				return err
			}
		}
		return nil
	})
}

// storedAppStale reads the stored :GoApplication.schema_version and
// max_level for THIS app and reports whether the content_hash diff cannot be
// trusted, so the caller upserts everything:
//
//   - a stored schema_version different from the producer's SchemaVersion;
//   - a stored max_level below this push's. A module's content_hash covers
//     its source, not the level it was analysed at, so an unchanged module
//     of a graph pushed at L3 still lacks what an L4 push adds to it — its L4
//     vertices are upserted (D5), but rows the diff skips stay at L3, and an
//     edge row whose endpoint is missing is dropped by its MATCH silently;
//   - a stored max_level present but not an integer: it cannot be compared,
//     so the push fails safe.
//
// No stored application (first push) forces nothing beyond the normal
// upsert, nor does a stored level at or above this push's.
func (w *boltWriter) storedAppStale() (bool, error) {
	var stored string
	var storedLevel int64
	haveLevel, badLevel := false, false
	err := w.read(func(tx neo4j.ManagedTransaction) error {
		res, err := tx.Run(w.ctx,
			"MATCH (a:GoApplication {id: $id}) RETURN a.schema_version AS v, a.max_level AS l, a.identity_version AS iv, a.application_name AS an, a.identity_namespace AS ns",
			map[string]any{"id": w.appID})
		if err != nil {
			return err
		}
		if res.Next(w.ctx) {
			iv, _ := res.Record().Get("iv")
			if n, ok := iv.(int64); !ok || n != 2 {
				return fmt.Errorf("incompatible stored identity_version: %v", iv)
			}
			nameValue, _ := res.Record().Get("an")
			namespaceValue, _ := res.Record().Get("ns")
			name, nameOK := nameValue.(string)
			namespace, namespaceOK := namespaceValue.(string)
			identity, identityErr := v2.ResolveIdentity(name, &namespace)
			if !nameOK || !namespaceOK || identityErr != nil || identity.ApplicationID != w.appID {
				return fmt.Errorf("incompatible stored application identity metadata")
			}

			if v, ok := res.Record().Get("v"); ok && v != nil {
				stored, _ = v.(string)
			}
			if l, ok := res.Record().Get("l"); ok && l != nil {
				storedLevel, haveLevel = l.(int64)
				badLevel = !haveLevel
			}
		}
		return res.Err()
	})
	if err != nil {
		return false, err
	}
	if stored != "" && stored != SchemaVersion || badLevel {
		return true, nil
	}
	return haveLevel && storedLevel < int64(w.level), nil
}

// ─── destructive steps (--eager only) ─────────────────────────────────────────

// eagerPurge runs as an implicit (auto-commit) transaction: Neo4j rejects
// CALL { ... } IN TRANSACTIONS inside an explicit one, which is what
// ExecuteWrite opens.
// shouldEagerPurge reports whether to delete the app's whole subgraph before
// pushing. The purge is app-wide, so it runs only on a full run: with
// --target-files the push carries only the targeted packages, and purging
// first would leave the rest of the app missing from the graph.
func (w *boltWriter) shouldEagerPurge(scoped bool) bool {
	return w.cfg.Eager && w.cfg.FullRun && scoped
}

func (w *boltWriter) eagerPurge(root string) error {
	q, params := eagerPurgeQuery(root)
	return w.autoCommit(q, params)
}

// purgeModule deletes the edges and declarations a module owned, scoped to its
// id prefix, so a re-analyzed module's stale facts are removed before re-upsert.
func (w *boltWriter) purgeModule(moduleID string) error {
	q, params := purgeModuleQuery(moduleID)
	return w.write(func(tx neo4j.ManagedTransaction) error {
		_, err := tx.Run(w.ctx, q, params)
		return err
	})
}

// eagerPurgeQuery / purgeModuleQuery are pure so the destructive-scope gate can
// assert they are anchored on :GoCanNode and the can:// prefix WITHOUT a live
// DB. Every destructive statement is scoped — never an unqualified DELETE.
func eagerPurgeQuery(root string) (string, map[string]any) {
	// Batched to avoid exhausting the transaction memory limit on large apps.
	return "MATCH (n:GoCanNode) WHERE n.id STARTS WITH $p OR n.id = $root " +
			"CALL (n) { DETACH DELETE n } IN TRANSACTIONS OF 5000 ROWS",
		map[string]any{"p": descendantPrefix(root), "root": root}
}

func purgeModuleQuery(moduleID string) (string, map[string]any) {
	return "MATCH (n:GoCanNode) WHERE n.id = $m OR n.id STARTS WITH $p DETACH DELETE n",
		map[string]any{"m": moduleID, "p": descendantPrefix(moduleID)}
}

// pruneVanishedModules removes modules under the app prefix that this run no
// longer emits (their source file vanished). Full + eager only.
func (w *boltWriter) pruneVanishedModules(present []string) error {
	root, _ := applicationPrefix(w.appID)
	desc := descendantPrefix(root)
	return w.write(func(tx neo4j.ManagedTransaction) error {
		_, err := tx.Run(w.ctx,
			"MATCH (m:GoModule) WHERE m.id STARTS WITH $p AND NOT m.id IN $present "+
				"MATCH (n:GoCanNode) WHERE n.id = m.id OR n.id STARTS WITH m.id + '/' DETACH DELETE n",
			map[string]any{"p": desc, "present": present})
		return err
	})
}

// ─── diff ─────────────────────────────────────────────────────────────────────

// moduleChanged compares a module's local content_hash against the DB's.
func (w *boltWriter) moduleChanged(id, localHash string) (bool, error) {
	var dbHash string
	err := w.read(func(tx neo4j.ManagedTransaction) error {
		res, err := tx.Run(w.ctx,
			"MATCH (m:GoModule {id: $id}) RETURN m.content_hash AS h",
			map[string]any{"id": id})
		if err != nil {
			return err
		}
		if res.Next(w.ctx) {
			if h, ok := res.Record().Get("h"); ok && h != nil {
				dbHash, _ = h.(string)
			}
		}
		return res.Err()
	})
	if err != nil {
		return false, err
	}
	// Changed when the hash differs or the module is new, or when either side
	// has no hash (can't prove unchanged → upsert).
	return localHash == "" || dbHash == "" || localHash != dbHash, nil
}

// ─── upserts ──────────────────────────────────────────────────────────────────

// upsertNodes and upsertEdges write a ready slice: buffer, then flush. They
// are test seams: the push path goes through the stager stages.
func (w *boltWriter) upsertNodes(nodes []NodeRow) error {
	if err := w.bufferNodes(nodes); err != nil {
		return err
	}
	return w.flushNodes()
}

func (w *boltWriter) upsertEdges(edges []EdgeRow) error {
	if err := w.bufferEdges(edges); err != nil {
		return err
	}
	return w.flushEdges()
}

// bufferNodes appends each node to its MERGE group and runs a group as soon
// as it holds boltBatchSize rows.
func (w *boltWriter) bufferNodes(nodes []NodeRow) error {
	if w.nodeBuf == nil {
		w.nodeBuf = map[string][]NodeRow{}
	}
	for _, n := range nodes {
		gk := nodeGroupKey(n)
		g := append(w.nodeBuf[gk], n)
		if len(g) == boltBatchSize {
			if err := w.runNodes(g); err != nil {
				return err
			}
			g = g[:0]
		}
		w.nodeBuf[gk] = g
	}
	return nil
}

// flushNodes runs every buffered node group in key order and clears the buffer.
func (w *boltWriter) flushNodes() error {
	for _, gk := range sortedKeys(w.nodeBuf) {
		if g := w.nodeBuf[gk]; len(g) > 0 {
			if err := w.runNodes(g); err != nil {
				return err
			}
		}
	}
	clear(w.nodeBuf)
	return nil
}

// runNodes upserts one chunk of a node group (same labels and key property).
func (w *boltWriter) runNodes(g []NodeRow) error {
	setLabels := ""
	for _, l := range g[0].Labels[1:] {
		setLabels += " SET n:" + l
	}
	q := fmt.Sprintf("UNWIND $rows AS row MERGE (n:%s {%s: row.k}) SET n += row.p%s",
		g[0].Labels[0], g[0].KeyProp, setLabels)
	return w.runBatch(q, nodeParams(g))
}

// bufferEdges appends each edge to its MERGE group and runs a group as soon
// as it holds boltBatchSize rows.
func (w *boltWriter) bufferEdges(edges []EdgeRow) error {
	if w.edgeBuf == nil {
		w.edgeBuf = map[string][]EdgeRow{}
	}
	for _, e := range edges {
		gk := edgeGroupKey(e)
		g := append(w.edgeBuf[gk], e)
		if len(g) == boltBatchSize {
			if err := w.runEdges(g); err != nil {
				return err
			}
			g = g[:0]
		}
		w.edgeBuf[gk] = g
	}
	return nil
}

// flushEdges runs every buffered edge group in key order and clears the buffer.
func (w *boltWriter) flushEdges() error {
	for _, gk := range sortedEdgeKeys(w.edgeBuf) {
		if g := w.edgeBuf[gk]; len(g) > 0 {
			if err := w.runEdges(g); err != nil {
				return err
			}
		}
	}
	clear(w.edgeBuf)
	return nil
}

// runEdges upserts one chunk of an edge group (same type, endpoint labels and
// keyedness).
func (w *boltWriter) runEdges(g []EdgeRow) error {
	e0 := g[0]
	keyed := e0.Key != ""
	var mergeClause string
	if keyed {
		mergeClause = fmt.Sprintf("MERGE (a)-[r:%s {_k: row.k, %s: row.f}]->(b)", e0.Type, RelationshipFactProperty)
	} else {
		mergeClause = fmt.Sprintf("MERGE (a)-[r:%s {%s: row.f}]->(b)", e0.Type, RelationshipFactProperty)
	}
	q := fmt.Sprintf("UNWIND $rows AS row MATCH (a:%s {%s: row.a}) MATCH (b:%s {%s: row.b}) %s SET r += row.p",
		e0.From.Label, e0.From.KeyProp, e0.To.Label, e0.To.KeyProp, mergeClause)
	return w.runBatch(q, edgeParams(g, keyed))
}

// runBatch runs one UNWIND statement over rows in its own write transaction.
func (w *boltWriter) runBatch(q string, rows []map[string]any) error {
	params := map[string]any{"rows": rows}
	return w.write(func(tx neo4j.ManagedTransaction) error {
		_, err := tx.Run(w.ctx, q, params)
		return err
	})
}

// ─── tx helpers ───────────────────────────────────────────────────────────────

func (w *boltWriter) write(fn func(neo4j.ManagedTransaction) error) error {
	_, err := w.session.ExecuteWrite(w.ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		return nil, fn(tx)
	})
	return err
}

// autoCommit runs one statement as an implicit transaction and consumes its
// result, so a server-side failure is returned here rather than dropped.
func (w *boltWriter) autoCommit(q string, params map[string]any) error {
	res, err := w.session.Run(w.ctx, q, params)
	if err != nil {
		return err
	}
	_, err = res.Consume(w.ctx)
	return err
}

func (w *boltWriter) read(fn func(neo4j.ManagedTransaction) error) error {
	_, err := w.session.ExecuteRead(w.ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		return nil, fn(tx)
	})
	return err
}

// ─── param marshalling ────────────────────────────────────────────────────────

// nodeParams converts node rows to driver params. Props map directly (the
// driver stores Go ints as integers, strings as strings, []string as a list).
// clearedAppProps are the :GoApplication props a push writes as an
// explicit null when the projection pruned them: `SET n += row.p` keeps a
// property row.p lacks, so a clean run over a graph a degraded run wrote
// would otherwise keep the stale degraded_json (F6). A null in SET +=
// removes the property. (The snapshot script wipes the application first.)
var clearedAppProps = []string{"degraded_json"}

func nodeParams(g []NodeRow) []map[string]any {
	out := make([]map[string]any, len(g))
	for i, n := range g {
		p := propsToAny(n.Props)
		if len(n.Labels) > 0 && n.Labels[0] == LabelApplication {
			for _, k := range clearedAppProps {
				if _, ok := p[k]; !ok {
					p[k] = nil
				}
			}
		}
		out[i] = map[string]any{"k": n.Value, "p": p}
	}
	return out
}

func edgeParams(g []EdgeRow, keyed bool) []map[string]any {
	out := make([]map[string]any, len(g))
	for i, e := range g {
		m := map[string]any{"a": e.From.Value, "b": e.To.Value, "p": propsToAny(e.Props), "f": edgeFactID(e)}
		if keyed {
			m["k"] = e.Key
		}
		out[i] = m
	}
	return out
}

func propsToAny(p Props) map[string]any {
	if len(p) == 0 {
		return map[string]any{}
	}
	out := make(map[string]any, len(p))
	for k, v := range p {
		out[k] = v
	}
	return out
}

// ─── partitioning & hashing ───────────────────────────────────────────────────

// partitionNodes splits nodes into the module-owned L1–L3 nodes (by owning
// module), the shared nodes (no module), and the L4 vertex nodes, owned by
// a module or by a stand-in, which a push always upserts (D5). It is a test
// seam: the push path partitions through replayRows and the stager stages.
func partitionNodes(nodes []NodeRow) (byModule map[string][]NodeRow, shared, l4 []NodeRow) {
	byModule = map[string][]NodeRow{}
	for _, n := range nodes {
		switch {
		case isL4Node(n):
			l4 = append(l4, n)
		case n.Module == "":
			shared = append(shared, n)
		default:
			byModule[n.Module] = append(byModule[n.Module], n)
		}
	}
	return byModule, shared, l4
}

// isL4Node reports an L4 vertex: a :GoBodyNode of a formal / actual kind.
func isL4Node(n NodeRow) bool {
	if n.Labels[0] != LabelBodyNode {
		return false
	}
	switch n.Props["kind"] {
	case "formal_in", "formal_out", "actual_in", "actual_out":
		return true
	}
	return false
}

// moduleHash returns the content_hash the projector put on the :GoModule node in
// this group (the module's own node carries it), else "".
func moduleHash(nodes []NodeRow) string {
	for _, n := range nodes {
		if n.Labels[0] == LabelModule {
			if h, ok := n.Props["content_hash"].(string); ok {
				return h
			}
		}
	}
	return ""
}

func sortedModuleKeys(m map[string][]NodeRow) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
