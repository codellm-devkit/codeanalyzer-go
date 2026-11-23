package core_test

import (
	"encoding/json"
	"testing"
)

// application.name is additive and carries the same value as application_name.
func TestAnalyze_ApplicationNameFieldMatchesLegacyField(t *testing.T) {
	dir := repoProject(t)
	var doc struct {
		Application struct {
			ApplicationName string `json:"application_name"`
			Name            string `json:"name"`
		} `json:"application"`
	}
	if err := json.Unmarshal(emitJSON(t, dir, false), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Application.Name == "" {
		t.Fatal("application.name missing")
	}
	if doc.Application.Name != doc.Application.ApplicationName {
		t.Errorf("name = %q, application_name = %q; want equal", doc.Application.Name, doc.Application.ApplicationName)
	}
	if doc.Application.Name != "repoapp" {
		t.Errorf("name = %q, want repoapp", doc.Application.Name)
	}
}
