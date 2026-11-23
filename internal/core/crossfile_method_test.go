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

// crossFileProject: T is declared in a.go, its method M in b.go, a package-level
// function M in b.go too (same name, different id), and Use in c.go calls both.
func crossFileProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"go.mod": "module example.com/xf\n\ngo 1.25.0\n",
		"a.go":   "package xf\n\ntype T struct{ n int }\n",
		"b.go":   "package xf\n\nfunc (t T) M() int { return t.n }\n\nfunc M() int { return 2 }\n",
		"c.go":   "package xf\n\nfunc Use() int {\n\tvar t T\n\treturn t.M() + M()\n}\n",
	}
	for rel, content := range files {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// analyzeL2 runs the real pipeline at -a 2 and returns the analysis.json tree as
// generic maps (the v2 Go structs are not needed for these checks).
func analyzeL2(t *testing.T, dir string) map[string]any {
	t.Helper()
	opts := options.AnalysisOptions{
		InputPath: dir, AppName: "xf", Level: options.LevelCallGraph,
		SkipTests: true, CacheDir: t.TempDir(), OutputDir: t.TempDir(),
	}
	app, err := core.New(opts).Analyze()
	if err != nil {
		t.Fatal(err)
	}
	identity, err := v2.ResolveIdentity("xf", nil)
	if err != nil {
		t.Fatal(err)
	}
	payload := v2emit.Emit(app, identity, dir, int(opts.Level), "test", 0, nil, nil)
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func asMap(v any) map[string]any { m, _ := v.(map[string]any); return m }

// methodNode finds the method whose signature ends in suffix, under any type.
func methodNode(st map[string]any, suffix string) map[string]any {
	for _, mod := range st {
		for _, ty := range asMap(asMap(mod)["types"]) {
			for _, c := range asMap(asMap(ty)["callables"]) {
				if strings.HasSuffix(asMap(c)["signature"].(string), suffix) {
					return asMap(c)
				}
			}
		}
	}
	return nil
}

func TestCrossFileMethod_IDNamesDeclaringFile(t *testing.T) {
	doc := analyzeL2(t, crossFileProject(t))
	st := asMap(asMap(doc["application"])["symbol_table"])

	m := methodNode(st, ".T.M")
	if m == nil {
		t.Fatal("method T.M not found")
	}
	const wantID = "can://xf/xf/go/b.go/T/M"
	if got := m["id"]; got != wantID {
		t.Errorf("method id = %v, want %s (declaring file b.go, not the receiver's a.go)", got, wantID)
	}
	if got := m["source_file"]; got != "b.go" {
		t.Errorf("source_file = %v, want b.go", got)
	}
}

func TestCrossFileMethod_SameNameFunctionDistinct(t *testing.T) {
	doc := analyzeL2(t, crossFileProject(t))
	st := asMap(asMap(doc["application"])["symbol_table"])
	fn := asMap(asMap(asMap(st["b.go"])["functions"]))
	var fnID string
	for _, f := range fn {
		if asMap(f)["signature"] == "example.com/xf.M" {
			fnID = asMap(f)["id"].(string)
		}
	}
	const wantFn = "can://xf/xf/go/b.go/M"
	if fnID != wantFn {
		t.Errorf("function M id = %q, want %s", fnID, wantFn)
	}
	if m := methodNode(st, ".T.M"); m != nil && m["id"] == fnID {
		t.Errorf("method and function named M collide at %s", fnID)
	}
}

// The call in Use (c.go) to the cross-file method resolves to the declaring-file
// id: the call-graph edge and the body call node both name b.go.
func TestCrossFileMethod_CallsResolveToDeclaringFile(t *testing.T) {
	doc := analyzeL2(t, crossFileProject(t))
	app := asMap(doc["application"])
	const method = "can://xf/xf/go/b.go/T/M"

	var caller string
	for _, mod := range asMap(app["symbol_table"]) {
		for _, f := range asMap(asMap(mod)["functions"]) {
			if asMap(f)["signature"] == "example.com/xf.Use" {
				caller = asMap(f)["id"].(string)
			}
		}
	}
	if caller == "" {
		t.Fatal("Use not found")
	}

	edge := false
	for _, e := range app["call_graph"].([]any) {
		em := asMap(e)
		if em["src"] == caller && em["dst"] == method {
			edge = true
		}
	}
	if !edge {
		t.Errorf("no call_graph edge %s -> %s", caller, method)
	}

	resolved := false
	for _, mod := range asMap(app["symbol_table"]) {
		for _, f := range asMap(asMap(mod)["functions"]) {
			if asMap(f)["id"] != caller {
				continue
			}
			for _, bn := range asMap(asMap(f)["body"]) {
				if c, ok := asMap(bn)["callee"].(string); ok && c == method {
					resolved = true
				}
			}
		}
	}
	if !resolved {
		t.Errorf("no body call node in Use has callee %s", method)
	}
}
