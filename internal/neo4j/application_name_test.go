package neo4j

import "testing"

// The :GoApplication node carries name alongside application_name, with the same value.
func TestProject_ApplicationNodeWritesName(t *testing.T) {
	a := repoAnalysis()
	a.Application.ApplicationName = "frontend"
	a.Application.Name = "frontend"

	var app *NodeRow
	for _, n := range Project(a).Nodes {
		if hasLabel(n, LabelApplication) {
			app = &n
		}
	}
	if app == nil {
		t.Fatal("no :GoApplication row")
	}
	if app.Props["name"] != "frontend" || app.Props["application_name"] != "frontend" {
		t.Errorf("props name=%v application_name=%v, want both frontend", app.Props["name"], app.Props["application_name"])
	}
}
