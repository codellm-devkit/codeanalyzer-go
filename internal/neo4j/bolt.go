package neo4j

import (
	"context"
	"fmt"
	"sort"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

// Milestone M5: the incremental Bolt writer. It pushes a GraphRows bag into a
// live Neo4j over Bolt, updating ONLY what changed, with the module subgraph as
// the unit of idempotent replacement. The neo4j driver is pure Go, so a
// CGO_ENABLED=0 static build still links; it is only exercised when EmitNeo4j
// runs with a --neo4j-uri, never on the JSON path.
//
// Algorithm:
//  1. ensure constraints + indexes;
//  2. stored-application gate: read :GoApplication.schema_version and
//     max_level scoped to this app's id; a version mismatch, or a stored level
//     below this push's, forces a full re-UPSERT (never a delete — that is
//     --eager's job);
//  3. partition nodes by owning module (NodeRow.module); shared nodes (externals,
//     neutral repo nodes) have none and are MERGE-only, always upserted; L4
//     vertex nodes (a callable's or a stand-in's formal/actual vertices) are
//     their own group;
//  4. --eager? scoped purge of everything under the app prefix, then rebuild;
//  5. diff each module's content_hash against the DB → the changed set;
//  6. per changed module: on --eager, purge its owned edges + stale decls, then
//     upsert its nodes; WITHOUT --eager the push only adds/updates;
//  7. upsert every L4 vertex node, changed module or not (D5, below);
//  8. upsert every edge (an unchanged edge's MERGE is a no-op);
//  9. on a full --eager run, orphan-prune modules whose source vanished.
//
// D5 (spec §4.11): L4 facts are not module-local — editing a callee changes
// its callers' summary edges, actual vertices and even formal_in indices —
// so the content_hash diff gates the L1–L3 rows only. A lazy push upserts
// every L4 row on every push: the vertex nodes (step 7), the stand-in
// bodies among them, and the GO_PARAM_IN / GO_PARAM_OUT / GO_SUMMARY rows
// and the port GO_DDG rows (prov ≠ ssa) with every other edge (step 8). It
// is additive: a stale L4 row stays until an --eager push.
//
// A push NEVER deletes by default (#171): steps 4/6/8 run under --eager only.
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

// WriteBolt pushes rows into the live DB per BoltConfig. appID scopes every
// destructive statement; it must be the application root id.
func WriteBolt(ctx context.Context, rows GraphRows, appID string, cfg BoltConfig) error {
	if err := validateIdentity(rows, appID); err != nil {
		return err
	}
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

	w := &boltWriter{ctx: ctx, session: session, rows: rows, appID: appID, cfg: cfg}
	return w.run()
}

type boltWriter struct {
	ctx     context.Context
	session neo4j.SessionWithContext
	rows    GraphRows
	appID   string
	cfg     BoltConfig
}

func (w *boltWriter) run() error {

	// Stored-application gate: a producer/consumer version mismatch, or a
	// graph pushed at a lower level, forces a full re-upsert (it never
	// deletes). The upsert path below is already a full MERGE of every
	// current node, so it simply means we don't trust the per-module
	// content_hash diff and upsert everything.
	forceFull, err := w.storedAppStale()
	if err != nil {
		return err
	}

	if err := w.ensureDDL(); err != nil {
		return err
	}
	root, scoped := applicationPrefix(w.appID)
	if w.shouldEagerPurge(scoped) {
		if err := w.eagerPurge(root); err != nil {
			return err
		}
	}

	// Partition nodes by owning module; shared nodes (module == "") and L4
	// vertex nodes are always upserted.
	byModule, shared, l4 := partitionNodes(w.rows.Nodes)

	// Diff content_hash per module unless a mismatch/eager forces everything.
	changed := map[string]bool{}
	if forceFull || w.cfg.Eager {
		for m := range byModule {
			changed[m] = true
		}
	} else {
		changed, err = w.changedModules(byModule)
		if err != nil {
			return err
		}
	}

	// Upsert shared nodes (MERGE-only, every run) then changed-module nodes.
	if err := w.upsertNodes(shared); err != nil {
		return err
	}
	for _, m := range sortedModuleKeys(byModule) {
		if !changed[m] {
			continue
		}
		if w.cfg.Eager && scoped {
			if err := w.purgeModule(m); err != nil {
				return err
			}
		}
		if err := w.upsertNodes(byModule[m]); err != nil {
			return err
		}
	}
	// L4 vertex nodes, after the module purges so an --eager purge of a
	// changed module cannot delete them (D5).
	if err := w.upsertNodes(l4); err != nil {
		return err
	}

	// Upsert every edge: the L4 rows must be (D5), and for the L1–L3 rows
	// the precise owner (the source node's module) buys nothing — a
	// MERGE-upsert of an unchanged edge is a harmless no-op.
	if err := w.upsertEdges(w.rows.Edges); err != nil {
		return err
	}

	if w.cfg.Eager && w.cfg.FullRun && scoped {
		if err := w.pruneVanishedModules(byModule); err != nil {
			return err
		}
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
			"MATCH (a:GoApplication {id: $id}) RETURN a.schema_version AS v, a.max_level AS l, a.identity_version AS iv",
			map[string]any{"id": w.appID})
		if err != nil {
			return err
		}
		if res.Next(w.ctx) {
			iv, _ := res.Record().Get("iv")
			if n, ok := iv.(int64); !ok || n != 2 {
				return fmt.Errorf("incompatible stored identity_version: %v", iv)
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
	return haveLevel && storedLevel < int64(w.pushLevel()), nil
}

// pushLevel is the max_level of the application row this push carries (0
// when it carries none).
func (w *boltWriter) pushLevel() int {
	for _, n := range w.rows.Nodes {
		if n.Labels[0] == LabelApplication && n.Value == w.appID {
			if l, ok := n.Props["max_level"].(int); ok {
				return l
			}
		}
	}
	return 0
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
func (w *boltWriter) pruneVanishedModules(byModule map[string][]NodeRow) error {
	present := make([]string, 0, len(byModule))
	for m := range byModule {
		present = append(present, m)
	}
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

// changedModules compares each module's content_hash in rows against the DB.
func (w *boltWriter) changedModules(byModule map[string][]NodeRow) (map[string]bool, error) {
	changed := map[string]bool{}
	for _, m := range sortedModuleKeys(byModule) {
		localHash := moduleHash(byModule[m])
		var dbHash string
		err := w.read(func(tx neo4j.ManagedTransaction) error {
			res, err := tx.Run(w.ctx,
				"MATCH (m:GoModule {id: $id}) RETURN m.content_hash AS h",
				map[string]any{"id": m})
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
			return nil, err
		}
		// Changed when the hash differs or the module is new, or when either side
		// has no hash (can't prove unchanged → upsert).
		if localHash == "" || dbHash == "" || localHash != dbHash {
			changed[m] = true
		}
	}
	return changed, nil
}

// ─── upserts ──────────────────────────────────────────────────────────────────

func (w *boltWriter) upsertNodes(nodes []NodeRow) error {
	if len(nodes) == 0 {
		return nil
	}
	groups := map[string][]NodeRow{}
	for _, n := range nodes {
		groups[nodeGroupKey(n)] = append(groups[nodeGroupKey(n)], n)
	}
	for _, gk := range sortedKeys(groups) {
		g := groups[gk]
		mergeLabel := g[0].Labels[0]
		keyProp := g[0].KeyProp
		extra := g[0].Labels[1:]
		setLabels := ""
		for _, l := range extra {
			setLabels += " SET n:" + l
		}
		q := fmt.Sprintf("UNWIND $rows AS row MERGE (n:%s {%s: row.k}) SET n += row.p%s",
			mergeLabel, keyProp, setLabels)
		params := map[string]any{"rows": nodeParams(g)}
		if err := w.write(func(tx neo4j.ManagedTransaction) error {
			_, err := tx.Run(w.ctx, q, params)
			return err
		}); err != nil {
			return err
		}
	}
	return nil
}

func (w *boltWriter) upsertEdges(edges []EdgeRow) error {
	if len(edges) == 0 {
		return nil
	}
	groups := map[string][]EdgeRow{}
	for _, e := range edges {
		groups[edgeGroupKey(e)] = append(groups[edgeGroupKey(e)], e)
	}
	for _, gk := range sortedEdgeKeys(groups) {
		g := groups[gk]
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
		params := map[string]any{"rows": edgeParams(g, keyed)}
		if err := w.write(func(tx neo4j.ManagedTransaction) error {
			_, err := tx.Run(w.ctx, q, params)
			return err
		}); err != nil {
			return err
		}
	}
	return nil
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
// a module or by a stand-in, which a push always upserts (D5).
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
