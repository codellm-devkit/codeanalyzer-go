package core_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// Two library callees can share one @external id. Here, package example.com/dep
// has type Box with method Put, and its sub-package example.com/dep/Box has
// function Put. Both would be example.com/dep/Box/Put. Before the fix, one
// external_symbols entry overwrote the other, so the method's call edge resolved
// to the function's metadata.
//
// Practical shape (the dependency sits outside the project and is replaced in):
//
//	dep/dep.go       type Box struct{};  func (Box) Put(s string) string
//	dep/Box/box.go   package box;        func Put(s string) string
//	proj/main.go     dep.Box{}.Put("a") ; box.Put("b")
func TestExternalIDs_TypeMethodAndFunctionKeepSeparateSymbols(t *testing.T) {
	base := t.TempDir()
	dep := filepath.Join(base, "dep")
	proj := filepath.Join(base, "proj")
	files := map[string]string{
		filepath.Join(dep, "go.mod"):        "module example.com/dep\n\ngo 1.22\n",
		filepath.Join(dep, "dep.go"):        "package dep\n\ntype Box struct{}\n\nfunc (Box) Put(s string) string { return \"method:\" + s }\n",
		filepath.Join(dep, "Box", "box.go"): "package box\n\nfunc Put(s string) string { return \"func:\" + s }\n",
		filepath.Join(proj, "go.mod"):       "module example.com/proj\n\ngo 1.22\n\nrequire example.com/dep v0.0.0\n\nreplace example.com/dep => " + dep + "\n",
		filepath.Join(proj, "main.go"): "package main\n\nimport (\n\t\"fmt\"\n\n\t\"example.com/dep\"\n\tbox \"example.com/dep/Box\"\n)\n\n" +
			"func main() {\n\tfmt.Println(dep.Box{}.Put(\"a\"), box.Put(\"b\"))\n}\n",
	}
	for path, content := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var doc struct {
		Application struct {
			ExternalSymbols map[string]struct {
				ID        string `json:"id"`
				Module    string `json:"module"`
				Signature string `json:"signature"`
			} `json:"external_symbols"`
			CallGraph []struct {
				Src string `json:"src"`
				Dst string `json:"dst"`
			} `json:"call_graph"`
		} `json:"application"`
	}
	if err := json.Unmarshal(emitCallGraphJSON(t, proj), &doc); err != nil {
		t.Fatal(err)
	}

	// Each callee needs its own symbol, with its own metadata.
	var methodID, funcID string
	for id, sym := range doc.Application.ExternalSymbols {
		if sym.ID != id {
			t.Errorf("symbol keyed %s carries id %s", id, sym.ID)
		}
		switch {
		case sym.Signature == "Box.Put" && sym.Module == "example.com/dep":
			methodID = id
		case sym.Signature == "Put" && sym.Module == "example.com/dep/Box":
			funcID = id
		}
	}
	if methodID == "" {
		t.Fatalf("no external symbol for method dep.Box.Put; have %+v", doc.Application.ExternalSymbols)
	}
	if funcID == "" {
		t.Fatalf("no external symbol for function box.Put; have %+v", doc.Application.ExternalSymbols)
	}
	if methodID == funcID {
		t.Fatalf("method and function share id %s", methodID)
	}

	// The two calls from main must reach their own symbols.
	var toMethod, toFunc bool
	for _, e := range doc.Application.CallGraph {
		if e.Dst == methodID {
			toMethod = true
		}
		if e.Dst == funcID {
			toFunc = true
		}
	}
	if !toMethod {
		t.Errorf("no call edge reaches the method symbol %s", methodID)
	}
	if !toFunc {
		t.Errorf("no call edge reaches the function symbol %s", funcID)
	}
}
