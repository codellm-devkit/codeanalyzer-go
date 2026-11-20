package neo4j

import (
	"testing"

	v2 "github.com/codellm-devkit/codeanalyzer-go/internal/schema/v2"
)

// repoAnalysis is a minimal L1 analysis carrying only the repository layer:
// go.mod and go.sum artifacts, and two dependencies (one locked, one not).
func repoAnalysis() *v2.Analysis {
	const app = "can://frontend/frontend"
	goMod := app + "/artifact/go.mod"
	goSum := app + "/artifact/go.sum"
	return &v2.Analysis{
		IdentityVersion: 2, SchemaVersion: v2.SchemaVersion, Language: v2.Language, MaxLevel: 1,
		Application: v2.Application{
			IdentityVersion: 2, ID: app, Kind: "application",
			Artifacts: map[string]v2.Artifact{
				"go.mod": {ID: goMod, Kind: "artifact", Path: "go.mod", Format: "gomod", Roles: []string{"dependency-manifest"}},
				"go.sum": {ID: goSum, Kind: "artifact", Path: "go.sum", Format: "gosum", Roles: []string{"lockfile"}},
			},
			Dependencies: []v2.Dependency{
				{Name: "github.com/gorilla/mux", Ecosystem: "golang", Spec: "v1.8.0", Kind: "runtime", Direct: true,
					LockedVersion: "v1.8.0", DeclaredIn: goMod, Prov: []string{"declared", "lockfile"}},
				{Name: "golang.org/x/text", Ecosystem: "golang", Spec: "v0.14.0", Kind: "runtime", Direct: false,
					DeclaredIn: goMod, Prov: []string{"declared"}},
			},
		},
	}
}

func TestProject_RepositoryLayerRows(t *testing.T) {
	rows := Project(repoAnalysis())

	nodes := map[string]NodeRow{}
	for _, n := range rows.Nodes {
		nodes[n.Value] = n
	}

	goMod := nodes["can://frontend/frontend/artifact/go.mod"]
	if !hasLabel(goMod, LabelArtifact) || !hasLabel(goMod, MarkerLabel) {
		t.Errorf("artifact node labels = %v, want Artifact + %s (app-scoped)", goMod.Labels, MarkerLabel)
	}
	mux := nodes["pkg:golang/github.com/gorilla/mux"]
	if !hasLabel(mux, LabelPackage) || hasLabel(mux, MarkerLabel) {
		t.Errorf("package node labels = %v, want Package only (neutral, no marker)", mux.Labels)
	}

	edges := map[string][]EdgeRow{}
	for _, e := range rows.Edges {
		edges[e.Type] = append(edges[e.Type], e)
	}
	if len(edges[RelHasArtifact]) != 2 {
		t.Errorf("HAS_ARTIFACT edges = %d, want 2 (go.mod, go.sum)", len(edges[RelHasArtifact]))
	}

	decl := edges[RelDeclaresDependency]
	if len(decl) != 2 {
		t.Fatalf("DECLARES_DEPENDENCY edges = %d, want 2", len(decl))
	}
	for _, e := range decl {
		if e.From.Value != "can://frontend/frontend/artifact/go.mod" || e.To.Label != LabelPackage {
			t.Errorf("DECLARES_DEPENDENCY wired %s -> %s:%s, want go.mod -> Package", e.From.Value, e.To.Label, e.To.Value)
		}
		if e.Key != "runtime" {
			t.Errorf("DECLARES_DEPENDENCY _k = %q, want runtime", e.Key)
		}
	}

	locks := edges[RelLocks]
	if len(locks) != 1 {
		t.Fatalf("LOCKS edges = %d, want 1 (only the locked dependency)", len(locks))
	}
	if locks[0].From.Value != "can://frontend/frontend/artifact/go.sum" ||
		locks[0].To.Value != "pkg:golang/github.com/gorilla/mux" ||
		locks[0].Props["locked_version"] != "v1.8.0" {
		t.Errorf("LOCKS = %+v, want go.sum -> mux with locked_version v1.8.0", locks[0])
	}
}

func TestProject_NoRepositoryLayerEmitsNoRows(t *testing.T) {
	a := repoAnalysis()
	a.Application.Artifacts, a.Application.Dependencies = nil, nil
	rows := Project(a)
	for _, n := range rows.Nodes {
		if hasLabel(n, LabelArtifact) || hasLabel(n, LabelPackage) {
			t.Errorf("repository node %v emitted without facts", n.Labels)
		}
	}
	for _, e := range rows.Edges {
		switch e.Type {
		case RelHasArtifact, RelDeclaresDependency, RelLocks:
			t.Errorf("repository edge %s emitted without facts", e.Type)
		}
	}
}
