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
