package core_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/codellm-devkit/codeanalyzer-go/internal/core"
	"github.com/codellm-devkit/codeanalyzer-go/internal/options"
)

// TestAnalyze_Level3RunsDataflow: Phase 5 fills the side table at -a 3 only,
// and a non-CLI caller that leaves FieldDepth 0 still gets k_limit 3.
func TestAnalyze_Level3RunsDataflow(t *testing.T) {
	opts := options.AnalysisOptions{
		InputPath: greeterDir(t), Level: options.LevelDataflow, SkipTests: true, CacheDir: t.TempDir(),
	}
	a := core.New(opts)
	app, err := a.Analyze()
	if err != nil {
		t.Fatal(err)
	}
	if len(a.L3()) == 0 {
		t.Fatal("-a 3 produced no dataflow results")
	}
	data, err := core.RenderJSON(app, a.L3(), opts)
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		KLimit int `json:"k_limit"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	if v.KLimit != 3 {
		t.Errorf("k_limit = %d, want the default 3", v.KLimit)
	}
	if !strings.Contains(string(data), `"cfg":[`) {
		t.Error("-a 3 JSON has no cfg")
	}

	opts.Level = options.LevelCallGraph
	a2 := core.New(opts)
	if _, err := a2.Analyze(); err != nil {
		t.Fatal(err)
	}
	if a2.L3() != nil {
		t.Error("-a 2 ran dataflow")
	}
}
