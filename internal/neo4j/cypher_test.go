package neo4j

import (
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
	return RenderCypher(rows, a.Application.ID)
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
	s1 := RenderCypher(Project(a), a.Application.ID)
	s2 := RenderCypher(Project(a), a.Application.ID)
	if s1 != s2 {
		t.Error("graph.cypher is not byte-identical across runs")
	}
}

func TestCypher_WipeIsPrefixScopedAndAnchored(t *testing.T) {
	script := renderFixture(t)
	// The wipe must match on the :GoCanNode marker and the can://<app>/ prefix,
	// never a bare label like (:GoModule) or the whole store.
	if !strings.Contains(script, "MATCH (n:GoCanNode) WHERE n.id STARTS WITH 'can://go/multipackage/'") {
		t.Errorf("wipe not anchored on :GoCanNode + descendant prefix:\n%s", wipeBlock(script))
	}
	if !strings.Contains(script, "MATCH (n:GoCanNode) WHERE n.id = 'can://go/multipackage'") {
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
	b.Node([]string{LabelCallable, SymbolLabel}, "id", "can://go/app/x.go/f", nil, "x.go")
	script := RenderCypher(b.Finish(), "can://go/app/x.go/f") // not a bare app id
	if strings.Contains(script, "DETACH DELETE") {
		t.Errorf("a bag with no bare application root must emit no wipe:\n%s", script)
	}
	if !strings.Contains(script, "scoped wipe SKIPPED") {
		t.Error("the skipped wipe should be visible in a comment")
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
