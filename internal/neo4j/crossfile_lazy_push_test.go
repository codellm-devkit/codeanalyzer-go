package neo4j

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codellm-devkit/codeanalyzer-go/internal/schema"
	v2 "github.com/codellm-devkit/codeanalyzer-go/internal/schema/v2"
	"github.com/codellm-devkit/codeanalyzer-go/internal/syntactic_analysis"
	"github.com/codellm-devkit/codeanalyzer-go/internal/syntactic_analysis/v2emit"
)

// The lazy push upserts a module's rows only when that module's content_hash
// changed. A cross-file method is declared in b.go but its receiver type lives in
// a.go, so before the fix its node and body rows were owned by a.go. Editing
// b.go left a.go's hash alone, so the method was never re-upserted and the graph
// kept its old body. These tests use real source on disk, analyzed by the real
// symbol-table builder and emitter, and project each version with Project.

// analyzeXF writes the files to a fresh project and returns its L1 analysis.
func analyzeXF(t *testing.T, files map[string]string) *v2.Analysis {
	t.Helper()
	dir := t.TempDir()
	for rel, content := range files {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	b := syntactic_analysis.NewSymbolTableBuilder(dir)
	st, err := b.Build(nil, true)
	if err != nil {
		t.Fatalf("symbol table build: %v", err)
	}
	app := &schema.GoApplication{SymbolTable: st}
	id, err := v2.ResolveIdentity("xf", nil)
	if err != nil {
		t.Fatal(err)
	}
	return v2emit.Emit(app, id, dir, 1, "lazy-push-test", 0, nil, nil)
}

// lazyUpsertIDs is the set of node ids a lazy (non-eager) push would upsert when
// the database holds the module hashes of the previous push. It mirrors the
// writer: shared and L4 nodes always, plus every node of a changed module.
func lazyUpsertIDs(rows GraphRows, prev GraphRows) map[string]bool {
	prevByModule, _, _ := partitionNodes(prev.Nodes)
	dbHash := map[string]string{}
	for m, nodes := range prevByModule {
		dbHash[m] = moduleHash(nodes)
	}
	byModule, shared, l4 := partitionNodes(rows.Nodes)
	changed := changedAgainst(byModule, func(m string) string { return dbHash[m] })
	up := map[string]bool{}
	for _, n := range shared {
		up[n.Value] = true
	}
	for _, n := range l4 {
		up[n.Value] = true
	}
	for m, nodes := range byModule {
		if !changed[m] {
			continue
		}
		for _, n := range nodes {
			up[n.Value] = true
		}
	}
	return up
}

// nodesUnder returns the ids of every node whose id is prefix or sits beneath it.
func nodesUnder(rows GraphRows, prefix string) []string {
	var out []string
	for _, n := range rows.Nodes {
		if n.Value == prefix || strings.HasPrefix(n.Value, prefix+"/") || strings.HasPrefix(n.Value, prefix+"@") {
			out = append(out, n.Value)
		}
	}
	return out
}

const xfA = "package xf\n\ntype T struct {\n\tn int\n}\n"

// Scenario 1: the method body changes in b.go; a.go is byte-identical.
//
//	a.go                     b.go (before)                       b.go (after)
//	type T struct{ n int }   func (t *T) M() int {               func (t *T) M() int {
//	                             return helper(t.n)                  return helper(t.n) + helper(2)
//	                         }                                    }
func TestLazyPush_EditingDeclaringFileUpsertsCrossFileMethod(t *testing.T) {
	before := analyzeXF(t, map[string]string{
		"go.mod": "module example.com/xf\n\ngo 1.25.0\n",
		"a.go":   xfA,
		"b.go":   "package xf\n\nfunc (t *T) M() int {\n\treturn helper(t.n)\n}\n\nfunc helper(n int) int { return n }\n",
	})
	after := analyzeXF(t, map[string]string{
		"go.mod": "module example.com/xf\n\ngo 1.25.0\n",
		"a.go":   xfA,
		"b.go":   "package xf\n\nfunc (t *T) M() int {\n\treturn helper(t.n) + helper(2)\n}\n\nfunc helper(n int) int { return n }\n",
	})
	rPrev, rNow := Project(before), Project(after)

	methodID := "can://xf/xf/go/b.go/T/M"
	up := lazyUpsertIDs(rNow, rPrev)
	missed := []string{}
	for _, id := range nodesUnder(rNow, methodID) {
		if !up[id] {
			missed = append(missed, id)
		}
	}
	if len(missed) > 0 {
		t.Errorf("lazy push skips rows of %s although b.go changed: %v", methodID, missed)
	}
}

// Scenario 2: a new method is added to b.go for T, which is declared in a.go.
// a.go is unchanged, so before the fix the new method's node and both edges that
// reference it were dropped: the MERGE edges MATCH the missing node and vanish.
//
//	b.go (after):  func (t *T) Reset() { t.n = 0 }   // T is declared in a.go
func TestLazyPush_NewCrossFileMethodIsUpserted(t *testing.T) {
	before := analyzeXF(t, map[string]string{
		"go.mod": "module example.com/xf\n\ngo 1.25.0\n",
		"a.go":   xfA,
		"b.go":   "package xf\n\nfunc (t *T) M() int { return t.n }\n",
	})
	after := analyzeXF(t, map[string]string{
		"go.mod": "module example.com/xf\n\ngo 1.25.0\n",
		"a.go":   xfA,
		"b.go":   "package xf\n\nfunc (t *T) M() int { return t.n }\n\nfunc (t *T) Reset() { t.n = 0 }\n",
	})
	rPrev, rNow := Project(before), Project(after)

	resetID := "can://xf/xf/go/b.go/T/Reset"
	up := lazyUpsertIDs(rNow, rPrev)
	if !up[resetID] {
		t.Errorf("lazy push drops new cross-file method %s (a.go unchanged, b.go changed)", resetID)
	}
	// Each edge that names the method must have its endpoint upserted, or the
	// MERGE in upsertEdges matches nothing and the edge is silently lost.
	for _, e := range rNow.Edges {
		if e.To.Value != resetID && e.From.Value != resetID {
			continue
		}
		for _, end := range []string{e.From.Value, e.To.Value} {
			if end == resetID && !up[end] {
				t.Errorf("edge %s %s -> %s has an un-upserted endpoint", e.Type, e.From.Value, e.To.Value)
			}
		}
	}
}

// The invariant the lazy push depends on: a module-owned row's id carries that
// module's id as a prefix, so the module that owns a row is the module whose
// content hash governs it. A cross-file method violated this before the fix.
func TestLazyPush_OwnerIsIdPrefixForEveryRow(t *testing.T) {
	a := analyzeXF(t, map[string]string{
		"go.mod": "module example.com/xf\n\ngo 1.25.0\n",
		"a.go":   xfA,
		"b.go":   "package xf\n\nfunc (t *T) M() int { return helper(t.n) }\n\nfunc (t *T) Reset() { t.n = 0 }\n\nfunc helper(n int) int { return n }\n",
	})
	for _, n := range Project(a).Nodes {
		if n.Module == "" || isL4Node(n) {
			continue
		}
		if !strings.HasPrefix(n.Value, n.Module) {
			t.Errorf("row %s %s is owned by %s, which is not its id prefix", n.Labels[0], n.Value, n.Module)
		}
	}
}
