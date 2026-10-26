//go:build neo4j_it

// Live-DB integration test for the Bolt writer. Gated behind the `neo4j_it`
// build tag so the default `go test ./...` (and CI without a database) skips it.
//
// Run against a disposable Neo4j:
//
//	docker run --rm -p7687:7687 -e NEO4J_AUTH=neo4j/testpass neo4j:5
//	NEO4J_TEST_URI=bolt://localhost:7687 NEO4J_TEST_PASS=testpass \
//	  go test -tags neo4j_it ./internal/neo4j/ -run Integration -v
//
// It asserts the behaviors the DB-free unit tests cannot: a push materializes
// the subgraph, a re-push is idempotent (same counts), a lazy push never
// deletes, and an eager push reconciles.
package neo4j

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

func itConfig(t *testing.T) BoltConfig {
	t.Helper()
	uri := os.Getenv("NEO4J_TEST_URI")
	if uri == "" {
		t.Skip("set NEO4J_TEST_URI to run the live Bolt integration test")
	}
	pass := os.Getenv("NEO4J_TEST_PASS")
	if pass == "" {
		pass = "neo4j"
	}
	return BoltConfig{URI: uri, User: "neo4j", Password: pass, Database: os.Getenv("NEO4J_TEST_DB")}
}

func TestIntegration_PushThenIdempotentRepush(t *testing.T) {
	ctx := context.Background()
	cfg := itConfig(t)
	cfg.Eager = true // start from a clean, reconciled state
	cfg.FullRun = true

	a := buildMultipackageL2(t)
	rows := Project(a)

	if err := WriteBolt(ctx, rows, a.Application.ID, cfg); err != nil {
		t.Fatalf("first push: %v", err)
	}
	n1, e1 := countSubgraph(t, ctx, cfg, a.Application.ID)

	// A second eager push of identical rows must yield identical counts.
	if err := WriteBolt(ctx, rows, a.Application.ID, cfg); err != nil {
		t.Fatalf("second push: %v", err)
	}
	n2, e2 := countSubgraph(t, ctx, cfg, a.Application.ID)

	if n1 != n2 || e1 != e2 {
		t.Errorf("re-push not idempotent: nodes %d->%d, edges %d->%d", n1, n2, e1, e2)
	}
	if n1 == 0 {
		t.Error("push materialized no nodes")
	}
}

func TestIntegration_LazyPushNeverDeletes(t *testing.T) {
	ctx := context.Background()
	cfg := itConfig(t)
	a := buildMultipackageL2(t)
	rows := Project(a)

	// Seed eagerly.
	seed := cfg
	seed.Eager, seed.FullRun = true, true
	if err := WriteBolt(ctx, rows, a.Application.ID, seed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	before, _ := countSubgraph(t, ctx, cfg, a.Application.ID)

	// A lazy push of an EMPTY row set must delete nothing.
	empty := GraphRows{}
	if err := WriteBolt(ctx, empty, a.Application.ID, cfg); err != nil {
		t.Fatalf("lazy empty push: %v", err)
	}
	after, _ := countSubgraph(t, ctx, cfg, a.Application.ID)
	if after < before {
		t.Errorf("lazy push deleted nodes: %d -> %d (lazy must be additive-only)", before, after)
	}
}

// countSubgraph opens a short-lived driver and counts the nodes and
// relationships under this app's can:// prefix (anchored on :GoCanNode).
func countSubgraph(t *testing.T, ctx context.Context, cfg BoltConfig, appID string) (nodes, edges int) {
	t.Helper()
	drv, err := neo4j.NewDriverWithContext(cfg.URI, neo4j.BasicAuth(cfg.User, cfg.Password, ""))
	if err != nil {
		t.Fatalf("driver: %v", err)
	}
	defer drv.Close(ctx)
	sess := drv.NewSession(ctx, neo4j.SessionConfig{DatabaseName: cfg.Database})
	defer sess.Close(ctx)

	root, _ := applicationPrefix(appID)
	desc := descendantPrefix(root)
	_, err = sess.ExecuteRead(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		nr, err := tx.Run(ctx,
			"MATCH (n:GoCanNode) WHERE n.id = $root OR n.id STARTS WITH $p RETURN count(n) AS c",
			map[string]any{"root": root, "p": desc})
		if err != nil {
			return nil, err
		}
		if nr.Next(ctx) {
			nodes = int(nr.Record().Values[0].(int64))
		}
		er, err := tx.Run(ctx,
			"MATCH (n:GoCanNode)-[r]->() WHERE n.id = $root OR n.id STARTS WITH $p RETURN count(r) AS c",
			map[string]any{"root": root, "p": desc})
		if err != nil {
			return nil, err
		}
		if er.Next(ctx) {
			edges = int(er.Record().Values[0].(int64))
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("count query: %v", err)
	}
	return nodes, edges
}

// TestIntegration_LazyPushRestoresL4Rows (D5): after an eager push of the
// -a 4 dataflow graph, delete its L4 rows (every GO_PARAM_IN / GO_PARAM_OUT
// / GO_SUMMARY relationship and every vertex body node); a lazy push of the
// same rows — every module unchanged, so its L1–L3 rows are skipped — puts
// every one of them back.
func TestIntegration_LazyPushRestoresL4Rows(t *testing.T) {
	ctx := context.Background()
	cfg := itConfig(t)
	a := buildDataflowL4(t)
	rows := Project(a)
	seed := cfg
	seed.Eager, seed.FullRun = true, true
	if err := WriteBolt(ctx, rows, a.Application.ID, seed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	want := l4Counts(t, ctx, cfg, a.Application.ID)
	for k, n := range want {
		if n == 0 {
			t.Fatalf("seeded graph has no %s", k)
		}
	}
	if want["GO_PARAM_IN"] != countEdges(rows, RelParamIn) || want["GO_SUMMARY"] != countEdges(rows, RelSummary) {
		t.Errorf("seeded L4 counts %v differ from the projection", want)
	}
	runIT(t, ctx, cfg, "MATCH (n:GoBodyNode) WHERE n.id STARTS WITH $p AND n.kind IN ['formal_in','formal_out','actual_in','actual_out'] DETACH DELETE n",
		map[string]any{"p": a.Application.ID + "/"})
	if got := l4Counts(t, ctx, cfg, a.Application.ID); got["vertex"] != 0 || got["GO_PARAM_IN"] != 0 {
		t.Fatalf("L4 rows not deleted: %v", got)
	}
	if err := WriteBolt(ctx, rows, a.Application.ID, cfg); err != nil {
		t.Fatalf("lazy push: %v", err)
	}
	if got := l4Counts(t, ctx, cfg, a.Application.ID); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("lazy push restored %v, want %v", got, want)
	}
}

func runIT(t *testing.T, ctx context.Context, cfg BoltConfig, q string, params map[string]any) {
	t.Helper()
	drv, err := neo4j.NewDriverWithContext(cfg.URI, neo4j.BasicAuth(cfg.User, cfg.Password, ""))
	if err != nil {
		t.Fatalf("driver: %v", err)
	}
	defer drv.Close(ctx)
	if _, err := neo4j.ExecuteQuery(ctx, drv, q, params, neo4j.EagerResultTransformer, neo4j.ExecuteQueryWithDatabase(cfg.Database)); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// l4Counts counts this app's L4 rows: vertex body nodes and the three L4
// relationship types, plus the port GO_DDG rows.
func l4Counts(t *testing.T, ctx context.Context, cfg BoltConfig, appID string) map[string]int {
	t.Helper()
	drv, err := neo4j.NewDriverWithContext(cfg.URI, neo4j.BasicAuth(cfg.User, cfg.Password, ""))
	if err != nil {
		t.Fatalf("driver: %v", err)
	}
	defer drv.Close(ctx)
	p := map[string]any{"p": appID + "/"}
	out := map[string]int{}
	for k, q := range map[string]string{
		"vertex":       "MATCH (n:GoBodyNode) WHERE n.id STARTS WITH $p AND n.kind IN ['formal_in','formal_out','actual_in','actual_out'] RETURN count(n)",
		"GO_PARAM_IN":  "MATCH (n:GoBodyNode)-[r:GO_PARAM_IN]->() WHERE n.id STARTS WITH $p RETURN count(r)",
		"GO_PARAM_OUT": "MATCH (n:GoBodyNode)-[r:GO_PARAM_OUT]->() WHERE n.id STARTS WITH $p RETURN count(r)",
		"GO_SUMMARY":   "MATCH (n:GoBodyNode)-[r:GO_SUMMARY]->() WHERE n.id STARTS WITH $p RETURN count(r)",
		"GO_DDG port":  "MATCH (n:GoBodyNode)-[r:GO_DDG]->() WHERE n.id STARTS WITH $p AND r.prov <> ['ssa'] RETURN count(r)",
	} {
		res, err := neo4j.ExecuteQuery(ctx, drv, q, p, neo4j.EagerResultTransformer, neo4j.ExecuteQueryWithDatabase(cfg.Database))
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		out[k] = int(res.Records[0].Values[0].(int64))
	}
	return out
}
