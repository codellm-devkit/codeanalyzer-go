package neo4j

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

// recordingSession is a DB-free SessionWithContext: it records which path a
// statement takes (implicit Run vs an explicit ExecuteWrite transaction).
// Methods it does not override panic through the nil embedded interface.
type recordingSession struct {
	neo4j.SessionWithContext
	autoCommit []string
	explicit   int
}

func (s *recordingSession) Run(_ context.Context, q string, _ map[string]any, _ ...func(*neo4j.TransactionConfig)) (neo4j.ResultWithContext, error) {
	s.autoCommit = append(s.autoCommit, q)
	return consumedResult{}, nil
}

func (s *recordingSession) ExecuteWrite(context.Context, neo4j.ManagedTransactionWork, ...func(*neo4j.TransactionConfig)) (any, error) {
	s.explicit++
	return nil, nil
}

type consumedResult struct{ neo4j.ResultWithContext }

func (consumedResult) Consume(context.Context) (neo4j.ResultSummary, error) { return nil, nil }

// TestEagerPurge_RunsAsImplicitTransaction: Neo4j 5 rejects CALL { … } IN
// TRANSACTIONS inside an explicit transaction (what ExecuteWrite opens), so
// the batched eager purge must go through the session's implicit Run path.
func TestEagerPurge_RunsAsImplicitTransaction(t *testing.T) {
	if q, _ := eagerPurgeQuery("can://app/app"); !strings.Contains(q, "IN TRANSACTIONS") {
		t.Fatalf("eagerPurgeQuery is not batched: %q", q)
	}
	s := &recordingSession{}
	w := &boltWriter{ctx: context.Background(), session: s}
	if err := w.eagerPurge("can://app/app"); err != nil {
		t.Fatal(err)
	}
	if s.explicit != 0 {
		t.Errorf("eagerPurge opened %d explicit transaction(s)", s.explicit)
	}
	if len(s.autoCommit) != 1 || !strings.Contains(s.autoCommit[0], "IN TRANSACTIONS") {
		t.Errorf("eagerPurge auto-commit statements = %q, want the one batched purge", s.autoCommit)
	}
}

// TestEagerPurge_OnlyOnFullRun: the app-wide purge deletes every node under the
// app prefix, so it must not run when --target-files limited the analysis to a
// few packages. Otherwise `--eager -t file.go` would wipe the app and push back
// only the targeted part.
func TestEagerPurge_OnlyOnFullRun(t *testing.T) {
	cases := []struct {
		eager, fullRun, scoped, want bool
	}{
		{eager: true, fullRun: true, scoped: true, want: true},
		{eager: true, fullRun: false, scoped: true, want: false},
		{eager: false, fullRun: true, scoped: true, want: false},
		{eager: true, fullRun: true, scoped: false, want: false},
	}
	for _, c := range cases {
		w := &boltWriter{cfg: BoltConfig{Eager: c.eager, FullRun: c.fullRun}}
		if got := w.shouldEagerPurge(c.scoped); got != c.want {
			t.Errorf("eager=%v fullRun=%v scoped=%v: shouldEagerPurge = %v, want %v",
				c.eager, c.fullRun, c.scoped, got, c.want)
		}
	}
}

// fakeDB is a DB-free SessionWithContext for the push algorithm: reads
// answer the stored-application gate with SchemaVersion and level (when
// non-zero; else no stored level) and the content_hash diff with
// hashes[module id] (an unchanged module when it equals the local hash);
// every write statement is recorded with its parameters.
type fakeDB struct {
	neo4j.SessionWithContext
	hashes map[string]string
	level  int64
	// rawLevel, when non-nil, is the stored max_level returned verbatim
	// (any type), overriding level.
	missingIdentity bool
	rawIdentity     any
	rawLevel        any
	writes          []fakeStmt
}

type fakeStmt struct {
	q      string
	params map[string]any
}

type fakeTx struct {
	neo4j.ManagedTransaction
	db *fakeDB
}

func (s *fakeDB) Run(_ context.Context, q string, params map[string]any, _ ...func(*neo4j.TransactionConfig)) (neo4j.ResultWithContext, error) {
	s.writes = append(s.writes, fakeStmt{q, params})
	return consumedResult{}, nil
}

func (s *fakeDB) ExecuteWrite(ctx context.Context, work neo4j.ManagedTransactionWork, _ ...func(*neo4j.TransactionConfig)) (any, error) {
	return work(fakeTx{db: s})
}

func (s *fakeDB) ExecuteRead(ctx context.Context, work neo4j.ManagedTransactionWork, _ ...func(*neo4j.TransactionConfig)) (any, error) {
	return work(fakeTx{db: s})
}

func (tx fakeTx) Run(_ context.Context, q string, params map[string]any) (neo4j.ResultWithContext, error) {
	switch {
	case strings.Contains(q, "a.schema_version"):
		var lvl any
		if tx.db.level != 0 {
			lvl = tx.db.level
		}
		if tx.db.rawLevel != nil {
			lvl = tx.db.rawLevel
		}
		identity := any(int64(2))
		if tx.db.missingIdentity {
			identity = nil
		}
		if tx.db.rawIdentity != nil {
			identity = tx.db.rawIdentity
		}
		return &fakeResult{rec: &neo4j.Record{Keys: []string{"v", "l", "iv"}, Values: []any{SchemaVersion, lvl, identity}}}, nil
	case strings.Contains(q, "m.content_hash"):
		h, ok := tx.db.hashes[params["id"].(string)]
		if !ok {
			return &fakeResult{}, nil
		}
		return &fakeResult{rec: &neo4j.Record{Keys: []string{"h"}, Values: []any{h}}}, nil
	}
	tx.db.writes = append(tx.db.writes, fakeStmt{q, params})
	return &fakeResult{}, nil
}

type fakeResult struct {
	neo4j.ResultWithContext
	rec  *neo4j.Record
	done bool
}

func (r *fakeResult) Next(context.Context) bool {
	if r.rec == nil || r.done {
		return false
	}
	r.done = true
	return true
}
func (r *fakeResult) Record() *neo4j.Record { return r.rec }
func (r *fakeResult) Err() error            { return nil }

// pushedIDs returns the node keys (row.k of node MERGEs) and the edge
// (type, a, b) triples a push wrote.
func (s *fakeDB) pushed() (nodes map[string]bool, edges map[string]int) {
	nodes, edges = map[string]bool{}, map[string]int{}
	for _, st := range s.writes {
		rows, _ := st.params["rows"].([]map[string]any)
		switch {
		case strings.Contains(st.q, "MERGE (n:"):
			for _, r := range rows {
				nodes[r["k"].(string)] = true
			}
		case strings.Contains(st.q, "MERGE (a)-[r:"):
			typ := st.q[strings.Index(st.q, "[r:")+3:]
			typ = typ[:strings.IndexAny(typ, " ]")]
			edges[typ] += len(rows)
		}
	}
	return nodes, edges
}

// TestBolt_LazyPushUpsertsL4RowsOfUnchangedModules (D5): on a lazy push
// where every module's content_hash matches the DB, the L1–L3 nodes of the
// (unchanged) modules are skipped, but every L4 row is upserted — each param
// vertex of a callable, each stand-in body node, and every GO_PARAM_IN /
// GO_PARAM_OUT / GO_SUMMARY row and port GO_DDG row (prov ≠ ssa). On an
// eager push nothing is skipped.
func TestBolt_LazyPushUpsertsL4RowsOfUnchangedModules(t *testing.T) {
	a := buildDataflowL4(t)
	rows := Project(a)
	byModule, _, _ := partitionNodes(rows.Nodes)
	hashes := map[string]string{}
	for m, ns := range byModule {
		hashes[m] = moduleHash(ns)
		if hashes[m] == "" {
			t.Fatalf("module %s has no content_hash", m)
		}
	}
	db := &fakeDB{hashes: hashes}
	w := &boltWriter{ctx: context.Background(), session: db, rows: rows, appID: a.Application.ID}
	if err := w.run(); err != nil {
		t.Fatal(err)
	}
	nodes, edges := db.pushed()

	var vertices, l3Skipped int
	for _, n := range rows.Nodes {
		if n.Labels[0] != LabelBodyNode {
			continue
		}
		kind, _ := n.Props["kind"].(string)
		switch {
		case isVertex(kind):
			vertices++
			if !nodes[n.Value] {
				t.Errorf("L4 vertex %s of an unchanged module not upserted", n.Value)
			}
		case n.Module != "":
			if nodes[n.Value] {
				t.Errorf("L3 body node %s of an unchanged module upserted on a lazy push", n.Value)
			}
			l3Skipped++
		}
	}
	if vertices == 0 || l3Skipped == 0 {
		t.Fatalf("fixture: %d vertices, %d L3 body nodes", vertices, l3Skipped)
	}
	standIns := 0
	for _, ext := range a.Application.ExternalSymbols {
		for _, n := range ext.Body {
			standIns++
			if !nodes[n.ID] {
				t.Errorf("stand-in vertex %s not upserted", n.ID)
			}
		}
	}
	if standIns == 0 {
		t.Fatal("fixture has no stand-in body")
	}
	portRows := 0
	for _, e := range rows.Edges {
		if p, _ := e.Props["prov"].([]string); e.Type == RelDDG && strings.Join(p, ",") != "ssa" {
			portRows++
		}
	}
	for _, rel := range []string{RelParamIn, RelParamOut, RelSummary, RelDDG} {
		if want := countEdges(rows, rel); want == 0 || edges[rel] != want {
			t.Errorf("%s: pushed %d rows, projected %d", rel, edges[rel], want)
		}
	}
	if portRows == 0 {
		t.Error("fixture has no port GO_DDG row")
	}

	// Eager: every node of every module is upserted.
	db = &fakeDB{hashes: hashes}
	w = &boltWriter{ctx: context.Background(), session: db, rows: rows, appID: a.Application.ID, cfg: BoltConfig{Eager: true, FullRun: true}}
	if err := w.run(); err != nil {
		t.Fatal(err)
	}
	nodes, _ = db.pushed()
	for _, n := range rows.Nodes {
		if !nodes[n.Value] {
			t.Fatalf("eager push skipped %s", n.Value)
		}
	}
}

// TestBolt_LazyPushForcesFullOnLowerStoredLevel: a graph first pushed at a
// lower level (an L3 push) has no L4 rows, and a lazy L4 push must not skip
// an unchanged module's L3 nodes: they may be missing L4 vertices' parents,
// and an edge MATCH on a missing endpoint drops the row silently. A stored
// max_level below the current one forces every module's upsert; an equal
// (or higher) stored level keeps the content_hash diff.
func TestBolt_LazyPushForcesFullOnLowerStoredLevel(t *testing.T) {
	a := buildDataflowL4(t)
	if a.MaxLevel != 4 {
		t.Fatalf("fixture max_level = %d, want 4", a.MaxLevel)
	}
	rows := Project(a)
	byModule, _, _ := partitionNodes(rows.Nodes)
	hashes := map[string]string{}
	for m, ns := range byModule {
		hashes[m] = moduleHash(ns)
	}
	push := func(stored int64) map[string]bool {
		db := &fakeDB{hashes: hashes, level: stored}
		w := &boltWriter{ctx: context.Background(), session: db, rows: rows, appID: a.Application.ID}
		if err := w.run(); err != nil {
			t.Fatal(err)
		}
		nodes, _ := db.pushed()
		return nodes
	}
	moduleNodes := 0
	for _, ns := range byModule {
		moduleNodes += len(ns)
	}
	if moduleNodes == 0 {
		t.Fatal("fixture has no module-owned nodes")
	}
	for _, stored := range []int64{1, 3} {
		nodes := push(stored)
		for m, ns := range byModule {
			for _, n := range ns {
				if !nodes[n.Value] {
					t.Fatalf("stored level %d: module %s node %s not upserted", stored, m, n.Value)
				}
			}
		}
	}
	for _, stored := range []int64{4, 5} {
		nodes := push(stored)
		for _, ns := range byModule {
			for _, n := range ns {
				if nodes[n.Value] {
					t.Fatalf("stored level %d: unchanged module node %s upserted", stored, n.Value)
				}
			}
		}
	}
}

// TestBolt_LazyPushForcesFullOnUnreadableStoredLevel: a stored max_level
// that is present but not an integer (a float, a string — a graph written by
// another tool, or hand-edited) cannot be compared with this push's level,
// so the lazy push fails safe and upserts every module, as for a lower one.
func TestBolt_LazyPushForcesFullOnUnreadableStoredLevel(t *testing.T) {
	a := buildDataflowL4(t)
	rows := Project(a)
	byModule, _, _ := partitionNodes(rows.Nodes)
	hashes := map[string]string{}
	for m, ns := range byModule {
		hashes[m] = moduleHash(ns)
	}
	for _, raw := range []any{float64(4), "4", true} {
		db := &fakeDB{hashes: hashes, rawLevel: raw}
		w := &boltWriter{ctx: context.Background(), session: db, rows: rows, appID: a.Application.ID}
		if err := w.run(); err != nil {
			t.Fatal(err)
		}
		nodes, _ := db.pushed()
		for m, ns := range byModule {
			for _, n := range ns {
				if !nodes[n.Value] {
					t.Fatalf("stored max_level %T(%v): module %s node %s not upserted", raw, raw, m, n.Value)
				}
			}
		}
	}
}

// TestBolt_CleanPushClearsDegradedJSON (F6): the projector prunes a nil
// degraded_json, and `SET n += row.p` keeps a property row.p lacks, so a
// clean push over a graph an earlier degraded run wrote would leave the
// stale degraded_json behind (and the SDK would refuse "no flow"
// certificates). The application row therefore carries an explicit null
// when nothing is degraded, which SET += removes; a degraded run keeps its
// JSON string.
func TestBolt_CleanPushClearsDegradedJSON(t *testing.T) {
	a := buildDataflowL4(t)
	if len(a.Degraded) != 0 {
		t.Fatalf("fixture degraded: %+v", a.Degraded)
	}
	appProps := func(rows GraphRows) map[string]any {
		t.Helper()
		db := &fakeDB{level: 4}
		w := &boltWriter{ctx: context.Background(), session: db, rows: rows, appID: a.Application.ID}
		if err := w.run(); err != nil {
			t.Fatal(err)
		}
		for _, st := range db.writes {
			if !strings.Contains(st.q, "MERGE (n:"+LabelApplication+" ") {
				continue
			}
			for _, r := range st.params["rows"].([]map[string]any) {
				if r["k"] == a.Application.ID {
					return r["p"].(map[string]any)
				}
			}
		}
		t.Fatal("no :GoApplication upsert")
		return nil
	}
	p := appProps(Project(a))
	if v, has := p["degraded_json"]; !has || v != nil {
		t.Errorf("clean push: degraded_json = %v (present %v), want an explicit null", v, has)
	}
	rows := Project(a)
	for i, n := range rows.Nodes {
		if n.Labels[0] == LabelApplication {
			rows.Nodes[i].Props = Props{"degraded_json": `[{"code":"no_vta"}]`, "max_level": int64(4)}
		}
	}
	if v := appProps(rows)["degraded_json"]; v != `[{"code":"no_vta"}]` {
		t.Errorf("degraded push: degraded_json = %v, want the JSON string", v)
	}
}

func TestIncompatibleIdentityReadOnly(t *testing.T) {
	for _, iv := range []any{int64(1), "2", float64(2), true} {
		db := &fakeDB{rawIdentity: iv}
		w := &boltWriter{ctx: context.Background(), session: db, appID: "can://team/app"}
		if err := w.run(); err == nil {
			t.Errorf("accepted %v", iv)
		}
		if len(db.writes) != 0 {
			t.Fatal("wrote before identity gate")
		}
	}
}

func TestMissingStoredIdentityReadOnly(t *testing.T) {
	db := &fakeDB{missingIdentity: true}
	w := &boltWriter{ctx: context.Background(), session: db, appID: "can://team/app"}
	if err := w.run(); err == nil {
		t.Fatal("accepted legacy anchor")
	}
	if len(db.writes) != 0 {
		t.Fatal("wrote before rejecting legacy anchor")
	}
}

func TestWritersPreserveDistinctEdgeFacts(t *testing.T) {
	for _, key := range []string{"", "original-key"} {
		for _, field := range []string{"site", "context"} {
			t.Run(key+"/"+field, func(t *testing.T) {
				build := func(reverse bool) GraphRows {
					b := NewRowBuilder()
					n := b.Node([]string{LabelCallable}, "id", "can://team/app/go/a", nil, "")
					sites := []string{"one", "two"}
					if reverse {
						sites[0], sites[1] = sites[1], sites[0]
					}
					for _, site := range sites {
						props := Props{"site": "shared", "context": "shared"}
						props[field] = site
						b.Edge(RelCalls, n, n, props, key)
					}
					return b.Finish()
				}
				var scripts []string
				var batches [][]map[string]any
				for _, reverse := range []bool{false, true} {
					rows := build(reverse)
					var script strings.Builder
					writeEdgeBatches(&script, rows.Edges)
					scripts = append(scripts, script.String())
					db := &fakeDB{}
					w := &boltWriter{ctx: context.Background(), session: db}
					if err := w.upsertEdges(rows.Edges); err != nil {
						t.Fatal(err)
					}
					if len(db.writes) != 1 {
						t.Fatalf("writes %d", len(db.writes))
					}
					q := db.writes[0].q
					if !strings.Contains(q, "_fact: row.f") || !strings.Contains(script.String(), "_fact: row.f") {
						t.Fatalf("fact identity missing:\n%s\n%s", q, script.String())
					}
					batch := db.writes[0].params["rows"].([]map[string]any)
					batches = append(batches, batch)
					if len(batch) != 2 || batch[0]["f"] == nil || batch[0]["f"] == batch[1]["f"] {
						t.Fatalf("collapsed facts: %+v", batch)
					}
					for _, row := range batch {
						f, ok := row["f"].(string)
						if !ok || !strings.Contains(script.String(), "f: "+cypherString(f)) {
							t.Fatalf("writer identity differs: %+v", row)
						}
						if key != "" {
							if row["k"] != key || !strings.Contains(q, "_k: row.k") || !strings.Contains(script.String(), "k: "+cypherString(key)) {
								t.Fatal("original key lost")
							}
						} else if _, ok := row["k"]; ok {
							t.Fatal("invented original key")
						}
					}
				}
				if scripts[0] != scripts[1] || !reflect.DeepEqual(batches[0], batches[1]) {
					t.Fatal("input order changes writer identities")
				}
			})
		}
	}
}
