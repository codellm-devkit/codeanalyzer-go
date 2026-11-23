package core_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codellm-devkit/codeanalyzer-go/internal/core"
	"github.com/codellm-devkit/codeanalyzer-go/internal/options"
	v2 "github.com/codellm-devkit/codeanalyzer-go/internal/schema/v2"
	"github.com/codellm-devkit/codeanalyzer-go/internal/syntactic_analysis/v2emit"
)

// initProject declares two func init() in one file (Go allows any number per
// file), one in another file, and each calls a helper.
func initProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"go.mod": "module example.com/ini\n\ngo 1.25.0\n",
		"main.go": "package ini\n\nvar registry = map[string]int{}\n\n" +
			"func a() { registry[\"a\"] = 1 }\nfunc b() { registry[\"b\"] = 2 }\n\n" +
			"func init() {\n\ta()\n}\n\n" +
			"func init() {\n\tb()\n}\n",
		"other.go": "package ini\n\nfunc init() {\n\ta()\n}\n",
	}
	for rel, content := range files {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// emitCallGraphJSON runs the analyzer at L2, where call_graph is emitted. The
// shared emitJSON helper runs at L1 and so has no edges to check.
func emitCallGraphJSON(t *testing.T, dir string) []byte {
	t.Helper()
	opts := options.AnalysisOptions{
		InputPath: dir, AppName: "repoapp", Level: options.LevelCallGraph,
		SkipTests: true, CacheDir: t.TempDir(), OutputDir: t.TempDir(),
	}
	app, err := core.New(opts).Analyze()
	if err != nil {
		t.Fatal(err)
	}
	identity, err := v2.ResolveIdentity("repoapp", nil)
	if err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(v2emit.Emit(app, identity, dir, int(opts.Level), "test", 0, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestInit_EveryInitSurvivesWithDistinctID guards the regression where a file
// with several func init() kept only the last one. Each init must appear in
// functions{} under its own id, and each must contribute its own call edge.
func TestInit_EveryInitSurvivesWithDistinctID(t *testing.T) {
	dir := initProject(t)
	var doc map[string]any
	if err := json.Unmarshal(emitCallGraphJSON(t, dir), &doc); err != nil {
		t.Fatal(err)
	}
	app := asMap(doc["application"])
	st := asMap(app["symbol_table"])

	var initIDs []string
	for _, mod := range st {
		for _, fn := range asMap(asMap(mod)["functions"]) {
			id, _ := asMap(fn)["id"].(string)
			if strings.HasSuffix(id, "/init") || strings.Contains(id, "/init:") {
				initIDs = append(initIDs, id)
			}
		}
	}
	if len(initIDs) != 3 {
		t.Fatalf("want 3 init callables (two in main.go, one in other.go), got %d: %v", len(initIDs), initIDs)
	}
	ids := map[string]bool{}
	for _, id := range initIDs {
		if ids[id] {
			t.Errorf("duplicate init id %s", id)
		}
		ids[id] = true
	}
	const base = "can://repoapp/repoapp/go/"
	for _, want := range []string{"main.go/init", "main.go/init:2", "other.go/init"} {
		if !ids[base+want] {
			t.Errorf("missing init id %s%s; have %v", base, want, initIDs)
		}
	}

	// Each init's own call is an edge from that init's id to its callee. The
	// two main.go inits must not merge into one edge set.
	wantEdges := map[string]bool{
		base + "main.go/init -> " + base + "main.go/a":   false,
		base + "main.go/init:2 -> " + base + "main.go/b": false,
		base + "other.go/init -> " + base + "main.go/a":  false,
	}
	cg, _ := app["call_graph"].([]any)
	for _, e := range cg {
		edge := asMap(e)
		key, _ := edge["src"].(string)
		dst, _ := edge["dst"].(string)
		key += " -> " + dst
		if _, ok := wantEdges[key]; ok {
			wantEdges[key] = true
		}
	}
	for edge, found := range wantEdges {
		if !found {
			t.Errorf("missing call-graph edge %s", edge)
		}
	}
}
