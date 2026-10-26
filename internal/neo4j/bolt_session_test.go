package neo4j

import (
	"context"
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
	if q, _ := eagerPurgeQuery("can://app"); !strings.Contains(q, "IN TRANSACTIONS") {
		t.Fatalf("eagerPurgeQuery is not batched: %q", q)
	}
	s := &recordingSession{}
	w := &boltWriter{ctx: context.Background(), session: s}
	if err := w.eagerPurge("can://app"); err != nil {
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
// answer the schema-version gate with SchemaVersion and the content_hash
// diff with hashes[module id] (an unchanged module when it equals the local
// hash); every write statement is recorded with its parameters.
type fakeDB struct {
	neo4j.SessionWithContext
	hashes map[string]string
	writes []fakeStmt
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
		return &fakeResult{rec: &neo4j.Record{Keys: []string{"v"}, Values: []any{SchemaVersion}}}, nil
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
