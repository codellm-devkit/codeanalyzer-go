package core_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// spanLinesProject declares one of each span-bearing model: an import, a type
// with a field, a method with a parameter, and a call in the method's body.
func spanLinesProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"go.mod": "module example.com/sl\n\ngo 1.25.0\n",
		"main.go": "package sl\n\nimport \"fmt\"\n\ntype T struct {\n\tn int\n}\n\n" +
			"func (t T) M(x int) int {\n\tfmt.Println(x)\n\treturn t.n\n}\n",
	}
	for rel, content := range files {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// checkSpanLines walks the JSON tree and asserts that every node carrying both a
// span and start_line/end_line has them equal to span.start[0] and span.end[0].
// seen counts those nodes by the innermost container they sit under.
func checkSpanLines(t *testing.T, v any, path string, seen map[string]int) {
	t.Helper()
	containers := []string{"imports", "types", "fields", "callables", "parameters", "body"}
	switch x := v.(type) {
	case map[string]any:
		if sp, ok := x["span"].(map[string]any); ok {
			start, _ := sp["start"].([]any)
			end, _ := sp["end"].([]any)
			if len(start) == 2 && len(end) == 2 {
				if sl, has := x["start_line"]; has {
					if sl != start[0] || x["end_line"] != end[0] {
						t.Errorf("%s: start_line/end_line = %v/%v, span = %v/%v", path, sl, x["end_line"], start[0], end[0])
					}
					if c := innermostContainer(path, containers); c != "" {
						seen[c]++
					}
				}
			}
		}
		for k, child := range x {
			checkSpanLines(t, child, path+"/"+k, seen)
		}
	case []any:
		for _, child := range x {
			checkSpanLines(t, child, path+"/[]", seen)
		}
	}
}

// innermostContainer is the deepest container segment on the path (a method's
// fields and parameters sit under its type, so the type is not the innermost).
func innermostContainer(path string, containers []string) string {
	segs := splitPath(path)
	for i := len(segs) - 1; i >= 0; i-- {
		for _, c := range containers {
			if segs[i] == c {
				return c
			}
		}
	}
	return ""
}

func splitPath(path string) []string {
	var out []string
	cur := ""
	for _, r := range path {
		if r == '/' {
			out = append(out, cur)
			cur = ""
			continue
		}
		cur += string(r)
	}
	return append(out, cur)
}

func TestSpanLines_KeysEqualSpanLines(t *testing.T) {
	dir := spanLinesProject(t)
	var doc map[string]any
	if err := json.Unmarshal(emitJSON(t, dir, false), &doc); err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	checkSpanLines(t, doc, "", seen)
	mods := asMap(asMap(asMap(doc["application"])["symbol_table"]))
	if len(mods) == 0 {
		t.Fatal("no modules in symbol_table")
	}
	for path, m := range mods {
		if _, ok := asMap(m)["start_line"]; !ok {
			t.Errorf("module %s has no start_line", path)
		}
	}
	for _, container := range []string{"imports", "types", "fields", "callables", "parameters", "body"} {
		if seen[container] == 0 {
			t.Errorf("no %s node carried start_line/end_line; saw %v", container, seen)
		}
	}
}
