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
