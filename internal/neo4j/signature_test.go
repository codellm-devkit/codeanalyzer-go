package neo4j

import (
	"testing"

	v2 "github.com/codellm-devkit/codeanalyzer-go/internal/schema/v2"
)

// The :GoCallable signature property is the JSON signature field, the full
// import-qualified name, not a segment parsed out of the id (the id now carries
// only the bare member name).
func TestProject_CallableSignatureFromJSONField(t *testing.T) {
	const (
		sig = "github.com/acme/app/worker.Worker.Run"
		mod = "can://ns/app/go/worker.go"
	)
	a := &v2.Analysis{
		IdentityVersion: 2, SchemaVersion: v2.SchemaVersion, Language: v2.Language, MaxLevel: 1,
		Application: v2.Application{
			IdentityVersion: 2, ID: "can://ns/app", Kind: "application",
			SymbolTable: map[string]v2.Module{
				"worker.go": {
					ID: mod, Kind: "module",
					Types: map[string]v2.Type{
						"Worker": {
							ID: mod + "/Worker", Kind: "struct", Signature: "github.com/acme/app/worker.Worker",
							Callables: map[string]v2.Callable{
								"Run": {ID: mod + "/Worker/Run", Kind: "method", Signature: sig},
							},
						},
					},
				},
			},
		},
	}
	rows := Project(a)
	for _, n := range rows.Nodes {
		if hasLabel(n, LabelCallable) && n.Value == mod+"/Worker/Run" {
			if got := n.Props["signature"]; got != sig {
				t.Errorf("signature property = %v, want %q", got, sig)
			}
			return
		}
	}
	t.Fatalf("no :GoCallable row for %s", mod+"/Worker/Run")
}

// The :GoType signature is the type JSON's signature field (full name), not the
// bare type name the id ends with.
func TestProject_TypeSignatureFromJSONField(t *testing.T) {
	const (
		sig = "github.com/acme/app/worker.Worker"
		mod = "can://ns/app/go/worker.go"
	)
	a := &v2.Analysis{
		IdentityVersion: 2, SchemaVersion: v2.SchemaVersion, Language: v2.Language, MaxLevel: 1,
		Application: v2.Application{
			IdentityVersion: 2, ID: "can://ns/app", Kind: "application",
			SymbolTable: map[string]v2.Module{
				"worker.go": {ID: mod, Kind: "module", Types: map[string]v2.Type{
					"Worker": {ID: mod + "/Worker", Kind: "struct", Signature: sig},
				}},
			},
		},
	}
	for _, n := range Project(a).Nodes {
		if hasLabel(n, LabelType) && n.Value == mod+"/Worker" {
			if got := n.Props["signature"]; got != sig {
				t.Errorf("type signature property = %v, want %q", got, sig)
			}
			return
		}
	}
	t.Fatalf("no :GoType row for %s", mod+"/Worker")
}
