package neo4j

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// M4 gate: the snapshot writer. The DDL + scoped wipe + batched node/edge MERGE
// blocks are present, deterministic, and prefix-scoped; a bag with no proper
// application root emits no destructive statement.

func renderFixture(t *testing.T) string {
	t.Helper()
	a := buildMultipackageL2(t)
	rows := Project(a)
	script, err := RenderCypher(rows, a.Application.ID)
	if err != nil {
		t.Fatal(err)
	}
	return script
}

func TestCypher_HasDDLWipeAndBatches(t *testing.T) {
	script := renderFixture(t)
	for _, want := range []string{
		"CREATE CONSTRAINT",
		"CREATE INDEX gocannode_id",
		"DETACH DELETE",
		"UNWIND [",
		"MERGE (n:",
		"MERGE (a)-[r:GO_",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q", want)
		}
	}
}

func TestCypher_Deterministic(t *testing.T) {
	a := buildMultipackageL2(t)
	s1, err := RenderCypher(Project(a), a.Application.ID)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := RenderCypher(Project(a), a.Application.ID)
	if err != nil {
		t.Fatal(err)
	}
	if s1 != s2 {
		t.Error("graph.cypher is not byte-identical across runs")
	}
}

func TestCypher_WipeIsPrefixScopedAndAnchored(t *testing.T) {
	script := renderFixture(t)
	// The wipe must match on the :GoCanNode marker and the can://<app>/ prefix,
	// never a bare label like (:GoModule) or the whole store.
	if !strings.Contains(script, "MATCH (n:GoCanNode) WHERE n.id STARTS WITH 'can://multipackage/multipackage/'") {
		t.Errorf("wipe not anchored on :GoCanNode + descendant prefix:\n%s", wipeBlock(script))
	}
	if !strings.Contains(script, "MATCH (n:GoCanNode) WHERE n.id = 'can://multipackage/multipackage'") {
		t.Errorf("wipe does not match the root id separately:\n%s", wipeBlock(script))
	}
	// A destructive statement must never be unscoped.
	for _, line := range strings.Split(script, "\n") {
		if strings.Contains(line, "DETACH DELETE") && !strings.Contains(line, "WHERE") {
			t.Errorf("unscoped DETACH DELETE: %q", line)
		}
	}
}

func TestCypher_NoRootEmitsNoWipe(t *testing.T) {
	// A hand-built bag with no bare application id must emit NO destructive
	// statement — the applicationPrefix refusal in action.
	b := NewRowBuilder()
	b.Node([]string{LabelCallable, SymbolLabel}, "id", "can://app/app/go/x.go/f", nil, "x.go")
	if _, err := RenderCypher(b.Finish(), "can://app/app/go/x.go/f"); err == nil {
		t.Fatal("accepted missing anchor")
	}
}

func TestCypher_SymbolFamilyMergesOnSharedLabel(t *testing.T) {
	// Nodes in the symbol family (labels[0]=GoSymbol) must MERGE on :GoSymbol and
	// SET the specific kind as an extra label.
	script := renderFixture(t)
	if !strings.Contains(script, "MERGE (n:GoSymbol {id: row.k})") {
		t.Error("symbol-family nodes must MERGE on the shared :GoSymbol label")
	}
	if !strings.Contains(script, "SET n:GoCallable") && !strings.Contains(script, "SET n:GoType") {
		t.Error("symbol-family nodes must SET their specific kind as an extra label")
	}
}

// wipeBlock extracts the wipe section for readable failure output.
func wipeBlock(script string) string {
	start := strings.Index(script, "scoped wipe")
	if start < 0 {
		return "(no wipe block)"
	}
	end := strings.Index(script[start:], "// ─── nodes")
	if end < 0 {
		end = len(script) - start
	}
	return script[start : start+end]
}

func TestCypherRejectsInvalidIdentity(t *testing.T) {
	a := buildMultipackageL2(t)
	a.IdentityVersion = 0
	if _, err := RenderCypher(Project(a), a.Application.ID); err == nil {
		t.Fatal("accepted missing identity marker")
	}
}

func TestEmitNeo4jInvalidIdentityLeavesOutput(t *testing.T) {
	a := buildMultipackageL2(t)
	a.Application.IdentityVersion = 1
	dir := t.TempDir()
	path := filepath.Join(dir, GraphFileName)
	if err := os.WriteFile(path, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := EmitNeo4j(context.Background(), TreeSource(a), dir, BoltConfig{}); err == nil {
		t.Fatal("accepted mismatched app marker")
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != "original" {
		t.Fatalf("output changed: %s %v", b, err)
	}
}

// TestCypher_StreamedScriptHasTheSameRows (S1): the streamed script holds the same row lines as
// RenderCypher(Project(a)), with the same header, and every node line comes before every edge line.
// A hand-built fixture with no identity metadata is refused by both.
func TestCypher_StreamedScriptHasTheSameRows(t *testing.T) {
	for name, a := range parityFixtures(t) {
		path := filepath.Join(t.TempDir(), GraphFileName)
		want, err := RenderCypher(Project(a), a.Application.ID)
		if err != nil {
			if _, _, werr := WriteCypher(treeSource(a), path); werr == nil {
				t.Errorf("%s: RenderCypher refuses (%v), WriteCypher accepts", name, err)
			}
			continue
		}
		if _, _, err := WriteCypher(treeSource(a), path); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		b, _ := os.ReadFile(path)
		got := string(b)
		head := want[:strings.Index(want, "UNWIND")]
		if !strings.HasPrefix(got, head) {
			t.Errorf("%s: header differs", name)
		}
		if w, g := rowLines(want), rowLines(got); !maps.Equal(w, g) {
			t.Errorf("%s: %d row lines, want %d", name, len(g), len(w))
		}
		if strings.LastIndex(got, "MERGE (n:") > strings.Index(got, "MERGE (a)-[r:") {
			t.Errorf("%s: a node statement follows an edge statement", name)
		}
	}
}

// rowLines is the set of UNWIND row lines, the lines that start with "  {".
func rowLines(script string) map[string]bool {
	out := map[string]bool{}
	for _, l := range strings.Split(script, "\n") {
		if strings.HasPrefix(l, "  {") {
			out[strings.TrimSuffix(l, ",")] = true
		}
	}
	return out
}

// TestWriteCypher_FailureLeavesOutput: a run that fails, before the temporary
// file (identity) or after it (rename onto a directory), leaves an earlier
// graph.cypher as it was and no temporary file behind.
func TestWriteCypher_FailureLeavesOutput(t *testing.T) {
	a := buildMultipackageL2(t)
	bad := *a
	bad.Application.IdentityVersion = 1
	dir := t.TempDir()
	path := filepath.Join(dir, GraphFileName)
	if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := WriteCypher(TreeSource(&bad), path); err == nil {
		t.Fatal("accepted mismatched app marker")
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "original" {
		t.Fatalf("output changed: %s %v", b, err)
	}
	// The rename fails: the target is a non-empty directory.
	target := filepath.Join(dir, "occupied")
	if err := os.MkdirAll(filepath.Join(target, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, _, err := WriteCypher(TreeSource(a), target)
	if err == nil || !strings.Contains(err.Error(), target) {
		t.Fatalf("err = %v, want a write error naming %s", err, target)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".graph-") {
			t.Errorf("temporary file %s left behind", e.Name())
		}
	}
}
