package v2emit

import (
	"github.com/codellm-devkit/codeanalyzer-go/internal/schema"
	v2 "github.com/codellm-devkit/codeanalyzer-go/internal/schema/v2"
)

// artifactID is an artifact's durable id: the application root, then the
// artifact segment and the repo-relative path (spec v2-artifacts).
func artifactID(appID, relPath string) string {
	return appID + "/artifact/" + relPath
}

// emitRepository maps the repository facts onto the v2 layer. Artifacts are
// keyed by path; dependencies keep the facts' order (sorted by module path) and
// point at their go.mod artifact by id. A nil facts set emits nothing.
func emitRepository(appID string, facts *schema.RepositoryFacts) (map[string]v2.Artifact, []v2.Dependency) {
	if facts == nil {
		return nil, nil
	}
	var arts map[string]v2.Artifact
	if len(facts.Artifacts) > 0 {
		arts = make(map[string]v2.Artifact, len(facts.Artifacts))
		for path, a := range facts.Artifacts {
			arts[path] = v2.Artifact{
				ID:         artifactID(appID, path),
				Kind:       "artifact",
				Path:       a.Path,
				Format:     a.Format,
				Roles:      a.Roles,
				SizeBytes:  a.SizeBytes,
				SHA256:     a.SHA256,
				Source:     a.Source,
				Extraction: a.Extraction,
			}
		}
	}
	var deps []v2.Dependency
	for _, d := range facts.Dependencies {
		declared := ""
		if d.DeclaredIn != "" {
			declared = artifactID(appID, d.DeclaredIn)
		}
		deps = append(deps, v2.Dependency{
			Name:            d.Name,
			Ecosystem:       d.Ecosystem,
			Spec:            d.Spec,
			Kind:            d.Kind,
			Direct:          d.Direct,
			LockedVersion:   d.LockedVersion,
			DeclaredIn:      declared,
			ProvidesImports: d.ProvidesImports,
			Prov:            d.Prov,
		})
	}
	return arts, deps
}
